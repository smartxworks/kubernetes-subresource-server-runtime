/*
Copyright 2021.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package subresourceserver

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	restfulspec "github.com/emicklei/go-restful-openapi/v2"
	"github.com/emicklei/go-restful/v3"
	openapispec "github.com/go-openapi/spec"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

func New(client kubernetes.Interface, opts ...ServerOption) Server {
	config := defaultServerConfig
	for _, opt := range opts {
		opt.apply(&config)
	}
	return &server{
		config: config,
		client: client,
	}
}

type Server interface {
	AddSubresource(subresource Subresource)
	Start(ctx context.Context) error
}

type server struct {
	config            serverConfig
	client            kubernetes.Interface
	userHeader        string
	groupHeader       string
	extraHeaderPrefix string
	subresources      []Subresource
}

var _ Server = &server{}

func (s *server) AddSubresource(subresource Subresource) {
	s.subresources = append(s.subresources, subresource)
}

func (s *server) Start(ctx context.Context) error {
	certFile := filepath.Join(s.config.certDir, s.config.certFileName)
	certFileExists := true
	if _, err := os.Stat(certFile); os.IsNotExist(err) {
		certFileExists = false
	}

	keyFile := filepath.Join(s.config.certDir, s.config.keyFileName)
	keyFileExists := true
	if _, err := os.Stat(keyFile); os.IsNotExist(err) {
		keyFileExists = false
	}

	if !certFileExists || !keyFileExists {
		if err := generateSelfSignedCert(certFile, keyFile); err != nil {
			return fmt.Errorf("generate self signed cert: %s", err)
		}
	}

	authInfo, err := s.client.CoreV1().ConfigMaps("kube-system").Get(ctx, "extension-apiserver-authentication", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get apiserver authentication config: %s", err)
	}

	clientCAs := x509.NewCertPool()
	clientCAs.AppendCertsFromPEM([]byte(authInfo.Data["requestheader-client-ca-file"]))

	s.userHeader = jsonUnmarshalFirstString([]byte(authInfo.Data["requestheader-username-headers"]))
	s.groupHeader = jsonUnmarshalFirstString([]byte(authInfo.Data["requestheader-group-headers"]))
	s.extraHeaderPrefix = jsonUnmarshalFirstString([]byte(authInfo.Data["requestheader-extra-headers-prefix"]))

	server := &http.Server{
		Addr:    s.config.addr,
		Handler: s.buildHandler(),
		TLSConfig: &tls.Config{
			ClientAuth: tls.RequireAndVerifyClientCert,
			ClientCAs:  clientCAs,
		},
	}

	idleConnsClosed := make(chan struct{})
	go func() {
		<-ctx.Done()
		if err := server.Shutdown(context.Background()); err != nil {
			log.Printf("error shutting down the HTTP server: %s", err)
		}
		close(idleConnsClosed)
	}()

	if err := server.ListenAndServeTLS(certFile, keyFile); err != nil && err != http.ErrServerClosed {
		return err
	}

	<-idleConnsClosed
	return nil
}

func (s *server) buildHandler() http.Handler {
	ws := &restful.WebService{}

	resourceLists := map[string][]metav1.APIResource{}
	versionLists := map[string][]metav1.GroupVersionForDiscovery{}
	for _, subresource := range s.subresources {
		subresource := subresource
		gvr := subresource.GetGroupVersionResource()
		gv := gvr.GroupVersion().String()
		var path string
		if subresource.IsNamespaceScoped() {
			path = fmt.Sprintf("/apis/%s/namespaces/{namespace}/%s/{name}/%s", gv, gvr.Resource, subresource.GetName())
		} else {
			path = fmt.Sprintf("/apis/%s/%s/{name}/%s", gv, gvr.Resource, subresource.GetName())
		}

		for _, method := range subresource.GetConnectMethods() {
			ws.Route(ws.Method(method).Path(path).To(func(req *restful.Request, resp *restful.Response) {
				var namespace string
				if subresource.IsNamespaceScoped() {
					namespace = req.PathParameter("namespace")
				}
				handler, err := subresource.Connect(req.Request.Context(), namespace, req.PathParameter("name"))
				if err != nil {
					resp.WriteError(http.StatusInternalServerError, err)
					return
				}
				handler.ServeHTTP(resp.ResponseWriter, req.Request)
			}))
		}

		resourceLists[gv] = append(resourceLists[gv], metav1.APIResource{
			Name:       fmt.Sprintf("%s/%s", gvr.Resource, subresource.GetName()),
			Namespaced: subresource.IsNamespaceScoped(),
		})

		versionLists[gvr.Group] = append(versionLists[gvr.Group], metav1.GroupVersionForDiscovery{
			GroupVersion: fmt.Sprintf("%s/%s", gvr.Group, gvr.Version),
			Version:      gvr.Version,
		})
	}

	var rootPaths []string
	for gv, resources := range resourceLists {
		gvPath := fmt.Sprintf("/apis/%s", gv)
		ws.Route(ws.GET(gvPath).To(func(req *restful.Request, resp *restful.Response) {
			resp.WriteAsJson(metav1.APIResourceList{
				GroupVersion: gv,
				APIResources: resources,
			})
		}))
		rootPaths = append(rootPaths, gvPath)
	}

	var groups []metav1.APIGroup
	for g, versions := range versionLists {
		groups = append(groups, metav1.APIGroup{
			Name: g,
			ServerAddressByClientCIDRs: []metav1.ServerAddressByClientCIDR{{
				ClientCIDR:    "0.0.0.0/0",
				ServerAddress: "",
			}},
			PreferredVersion: versions[len(versions)-1],
			Versions:         versions,
		})
	}
	ws.Route(ws.GET("/apis").To(func(req *restful.Request, resp *restful.Response) {
		resp.WriteAsJson(metav1.APIGroupList{
			Groups: groups,
		})
	}))
	rootPaths = append(rootPaths, "/apis")

	var spec *openapispec.Swagger
	specOnce := sync.Once{}
	ws.Route(ws.GET("/openapi/v2").To(func(req *restful.Request, resp *restful.Response) {
		specOnce.Do(func() {
			spec = restfulspec.BuildSwagger(restfulspec.Config{
				WebServices:    []*restful.WebService{ws},
				WebServicesURL: "",
				APIPath:        "/swaggerapi",
			})
		})
		resp.WriteAsJson(spec)
	}))
	rootPaths = append(rootPaths, "/openapi/v2")

	ws.Route(ws.GET("/").To(func(req *restful.Request, resp *restful.Response) {
		resp.WriteAsJson(metav1.RootPaths{
			Paths: rootPaths,
		})
	}))

	wsc := restful.NewContainer()
	wsc.Add(ws)
	wsc.Filter(restful.OPTIONSFilter())
	wsc.Filter(func(req *restful.Request, resp *restful.Response, chain *restful.FilterChain) {
		segs := strings.Split(req.Request.URL.Path, "/")
		if len(segs) != 9 {
			chain.ProcessFilter(req, resp)
			return
		}

		group := segs[2]
		version := segs[3]
		namespace := segs[5]
		resource := segs[6]
		name := segs[7]
		subresource := segs[8]

		var verb string
		switch req.Request.Method {
		case http.MethodGet:
			verb = "get"
		case http.MethodPost:
			verb = "create"
		case http.MethodPut:
			verb = "update"
		case http.MethodPatch:
			verb = "patch"
		case http.MethodDelete:
			verb = "delete"
		default:
			resp.WriteErrorString(http.StatusMethodNotAllowed, http.StatusText(http.StatusMethodNotAllowed))
			return
		}

		user := req.Request.Header.Get(s.userHeader)
		groups := req.Request.Header[s.groupHeader]
		extras := map[string]authorizationv1.ExtraValue{}
		for k, v := range req.Request.Header {
			if strings.HasPrefix(k, s.extraHeaderPrefix) {
				extras[strings.TrimPrefix(k, s.extraHeaderPrefix)] = v
			}
		}

		sar := &authorizationv1.SubjectAccessReview{
			Spec: authorizationv1.SubjectAccessReviewSpec{
				User:   user,
				Groups: groups,
				Extra:  extras,
				ResourceAttributes: &authorizationv1.ResourceAttributes{
					Namespace:   namespace,
					Verb:        verb,
					Group:       group,
					Version:     version,
					Resource:    resource,
					Subresource: subresource,
					Name:        name,
				},
			},
		}
		result, err := s.client.AuthorizationV1().SubjectAccessReviews().Create(req.Request.Context(), sar, metav1.CreateOptions{})
		if err != nil {
			resp.WriteError(http.StatusInternalServerError, err)
			return
		}
		if !result.Status.Allowed {
			resp.WriteErrorString(http.StatusUnauthorized, http.StatusText(http.StatusUnauthorized))
			return
		}

		chain.ProcessFilter(req, resp)
	})
	return wsc
}

func generateSelfSignedCert(certFile string, keyFile string) error {
	key, err := rsa.GenerateKey(rand.Reader, 4096)
	if err != nil {
		return fmt.Errorf("generate key: %s", err)
	}

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(time.Hour * 24 * 180),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	cert, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("create cert: %s", err)
	}

	if err := os.MkdirAll(filepath.Dir(certFile), 0755); err != nil {
		return fmt.Errorf("create cert file dir: %s", err)
	}
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert}), 0644); err != nil {
		return fmt.Errorf("write cert file: %s", err)
	}

	if err := os.MkdirAll(filepath.Dir(keyFile), 0755); err != nil {
		return fmt.Errorf("create key file dir: %s", err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0400); err != nil {
		return fmt.Errorf("write key file: %s", err)
	}
	return nil
}

func jsonUnmarshalFirstString(data []byte) string {
	var v []string
	if err := json.Unmarshal(data, &v); err != nil {
		panic(err)
	}
	if len(v) == 0 {
		return ""
	}
	return v[0]
}
