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
	"github.com/gorilla/mux"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

func New(client kubernetes.Interface, opts ...ServerOption) *Server {
	config := defaultServerConfig
	for _, opt := range opts {
		opt.apply(&config)
	}
	return &Server{
		config: config,
		client: client,
	}
}

type Server struct {
	config            serverConfig
	client            kubernetes.Interface
	userHeader        string
	groupHeader       string
	extraHeaderPrefix string
	subresources      []*Subresource
}

func (s *Server) AddSubresource(r *Subresource) {
	s.subresources = append(s.subresources, r)
}

func (s *Server) Start(ctx context.Context) error {
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
			GetCertificate: func(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
				cert, err := tls.LoadX509KeyPair(certFile, keyFile)
				if err != nil {
					return nil, err
				}
				return &cert, nil
			},
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

	if err := server.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
		return err
	}

	<-idleConnsClosed
	return nil
}

func (s *Server) buildHandler() http.Handler {
	ws := &restful.WebService{}

	resourceLists := map[string][]metav1.APIResource{}
	versionLists := map[string][]metav1.GroupVersionForDiscovery{}
	for _, r := range s.subresources {
		r := r
		for _, method := range r.ConnectMethods {
			keyPlacehodler := types.NamespacedName{
				Namespace: "{namespace}",
				Name:      "{name}",
			}
			path := r.Path(keyPlacehodler)
			ws.Route(ws.Method(method).Path(path).To(func(req *restful.Request, resp *restful.Response) {
				key := types.NamespacedName{
					Namespace: req.PathParameter("namespace"),
					Name:      req.PathParameter("name"),
				}
				handler, err := r.Connect(req.Request.Context(), key)
				if err != nil {
					resp.WriteError(http.StatusInternalServerError, err)
					return
				}
				handler.ServeHTTP(resp.ResponseWriter, req.Request)
			}))
		}

		group := r.GroupVersionResource.Group
		version := r.GroupVersionResource.Version
		groupVersion := fmt.Sprintf("%s/%s", group, version)
		resourceLists[groupVersion] = append(resourceLists[groupVersion], metav1.APIResource{
			Name:       fmt.Sprintf("%s/%s", r.GroupVersionResource.Resource, r.Name),
			Namespaced: r.NamespaceScoped,
		})
		versionLists[group] = append(versionLists[group], metav1.GroupVersionForDiscovery{
			GroupVersion: groupVersion,
			Version:      version,
		})
	}

	var rootPaths []string
	for groupVersion, resources := range resourceLists {
		path := fmt.Sprintf("/apis/%s", groupVersion)
		ws.Route(ws.GET(path).Produces(restful.MIME_JSON).To(func(req *restful.Request, resp *restful.Response) {
			resp.WriteAsJson(metav1.APIResourceList{
				GroupVersion: groupVersion,
				APIResources: resources,
			})
		}))
		rootPaths = append(rootPaths, path)
	}

	var groups []metav1.APIGroup
	for group, versions := range versionLists {
		groups = append(groups, metav1.APIGroup{
			Name: group,
			ServerAddressByClientCIDRs: []metav1.ServerAddressByClientCIDR{{
				ClientCIDR:    "0.0.0.0/0",
				ServerAddress: "",
			}},
			PreferredVersion: versions[len(versions)-1],
			Versions:         versions,
		})
	}
	ws.Route(ws.GET("/apis").Produces(restful.MIME_JSON).To(func(req *restful.Request, resp *restful.Response) {
		resp.WriteAsJson(metav1.APIGroupList{
			Groups: groups,
		})
	}))
	rootPaths = append(rootPaths, "/apis")

	var spec *openapispec.Swagger
	specOnce := sync.Once{}
	ws.Route(ws.GET("/openapi/v2").Produces(restful.MIME_JSON).To(func(req *restful.Request, resp *restful.Response) {
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

	ws.Route(ws.GET("/").Produces(restful.MIME_JSON).To(func(req *restful.Request, resp *restful.Response) {
		resp.WriteAsJson(metav1.RootPaths{
			Paths: rootPaths,
		})
	}))

	wsc := restful.NewContainer()
	wsc.Add(ws)
	wsc.Filter(restful.OPTIONSFilter())
	wsc.Filter(func(req *restful.Request, resp *restful.Response, chain *restful.FilterChain) {
		segs := strings.Split(req.Request.URL.Path, "/")
		if len(segs) < 9 {
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

	m := mux.NewRouter()
	for _, r := range s.subresources {
		if r.Route == nil {
			continue
		}

		keyPlacehodler := types.NamespacedName{
			Namespace: "{namespace}",
			Name:      "{name}",
		}
		path := r.Path(keyPlacehodler)
		m.PathPrefix(fmt.Sprintf("%s/", path)).HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			vars := mux.Vars(req)
			key := types.NamespacedName{
				Namespace: vars["namespace"],
				Name:      vars["name"],
			}
			relPath := strings.TrimPrefix(req.URL.Path, r.Path(key))
			handler, err := r.Route(req.Context(), key, relPath)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			handler.ServeHTTP(w, req)
		})
	}
	m.PathPrefix("").Handler(wsc)
	return m
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
