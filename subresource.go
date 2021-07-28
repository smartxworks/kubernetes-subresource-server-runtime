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
	"fmt"
	"net/http"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

type Subresource struct {
	NamespaceScoped      bool
	GroupVersionResource schema.GroupVersionResource
	Name                 string
	ConnectMethods       []string
	Connect              func(ctx context.Context, key types.NamespacedName) (http.Handler, error)
	Route                func(ctx context.Context, key types.NamespacedName, path string) (http.Handler, error)
}

func (r *Subresource) Path(key types.NamespacedName) string {
	var path string
	if r.NamespaceScoped {
		path = fmt.Sprintf("/apis/%s/%s/namespaces/%s/%s/%s/%s",
			r.GroupVersionResource.Group, r.GroupVersionResource.Version, key.Namespace,
			r.GroupVersionResource.Resource, key.Name, r.Name)
	} else {
		path = fmt.Sprintf("/apis/%s/%s/%s/%s/%s",
			r.GroupVersionResource.Group, r.GroupVersionResource.Version,
			r.GroupVersionResource.Resource, key.Name, r.Name)
	}
	return path
}
