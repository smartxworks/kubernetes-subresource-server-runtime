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

package main

import (
	"context"
	"log"
	"net/http"

	"k8s.io/apimachinery/pkg/runtime/schema"
	genericserver "k8s.io/apiserver/pkg/server"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	subresourceserver "github.com/smartxworks/kubernetes-subresource-server-runtime"
)

func main() {
	cfg, err := clientcmd.BuildConfigFromFlags("", "")
	if err != nil {
		log.Fatalf("failed to build kubeconfig: %s", err)
	}

	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("failed to create client: %s", err)
	}

	s := subresourceserver.New(client)
	s.AddSubresource(&FooBar{})

	if err := s.Start(genericserver.SetupSignalContext()); err != nil {
		log.Fatalf("error running server: %s", err)
	}
}

type FooBar struct {
}

var _ subresourceserver.Subresource = &FooBar{}

func (b *FooBar) IsNamespaceScoped() bool {
	return true
}

func (b *FooBar) GetGroupVersionResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    "subresource.example.org",
		Version:  "v1alpha1",
		Resource: "foos",
	}
}

func (b *FooBar) GetName() string {
	return "bar"
}

func (b *FooBar) GetConnectMethods() []string {
	return []string{http.MethodGet}
}

func (b *FooBar) Connect(ctx context.Context, namespace string, name string) (http.Handler, error) {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("Hello, World!"))
	}), nil
}
