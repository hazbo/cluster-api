/*
Copyright 2019 The Kubernetes Authors.

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

package repository

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/pkg/errors"
	appsv1 "k8s.io/api/apps/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	clusterctlv1 "sigs.k8s.io/cluster-api/cmd/clusterctl/api/v1alpha3"
	"sigs.k8s.io/cluster-api/cmd/clusterctl/pkg/client/config"
	"sigs.k8s.io/cluster-api/cmd/clusterctl/pkg/internal/scheme"
	"sigs.k8s.io/cluster-api/cmd/clusterctl/pkg/internal/util"
)

// variableRegEx defines the regexp used for searching variables inside a YAML
var variableRegEx = regexp.MustCompile(`\${\s*([A-Z0-9_]+)\s*}`)

// Components wraps a YAML file that defines the provider components
// to be installed in a management cluster (CRD, Controller, RBAC etc.)
// It is important to notice that clusterctl applies a set of processing steps to the “raw” component YAML read
// from the provider repositories:
// 1. Checks for all the variables in the component YAML file and replace with corresponding config values
// 2. Ensure all the provider components are deployed in the target namespace (apply only to namespaced objects)
// 3. Ensure all the ClusterRoleBinding which are referencing namespaced objects have the name prefixed with the namespace name
// 4. Set the watching namespace for the provider controller
// 5. Adds labels to all the components in order to allow easy identification of the provider objects
type Components interface {
	// configuration of the provider the template belongs to.
	config.Provider

	// Version of the provider.
	Version() string

	// Variables required by the template.
	// This value is derived by the component YAML.
	Variables() []string

	// TargetNamespace where the provider components will be installed.
	// By default this value is derived by the component YAML, but it is possible to override it
	// during the creation of the Components object.
	TargetNamespace() string

	// WatchingNamespace defines the namespace where the provider controller is is watching (empty means all namespaces).
	// By default this value is derived by the component YAML, but it is possible to override it
	// during the creation of the Components object.
	WatchingNamespace() string

	// Metadata returns the clusterctl metadata object representing the provider that will be
	// generated by this provider components.
	Metadata() clusterctlv1.Provider

	// Yaml return the provider components in the form of a YAML file.
	Yaml() ([]byte, error)

	// Objs return the provider components in the form of a list of Unstructured objects.
	Objs() []unstructured.Unstructured
}

// components implement Components
type components struct {
	config.Provider
	version           string
	variables         []string
	targetNamespace   string
	watchingNamespace string
	objs              []unstructured.Unstructured
}

// ensure components implement Components
var _ Components = &components{}

func (c *components) Version() string {
	return c.version
}

func (c *components) Variables() []string {
	return c.variables
}

func (c *components) TargetNamespace() string {
	return c.targetNamespace
}

func (c *components) WatchingNamespace() string {
	return c.watchingNamespace
}

func (c *components) Metadata() clusterctlv1.Provider {
	return clusterctlv1.Provider{
		TypeMeta: metav1.TypeMeta{
			APIVersion: clusterctlv1.GroupVersion.String(),
			Kind:       "Provider",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: c.targetNamespace,
			Name:      c.Name(),
			Labels:    getLabels(c.Name()),
		},
		Type:             string(c.Type()),
		Version:          c.version,
		WatchedNamespace: c.watchingNamespace,
	}
}

func (c *components) Objs() []unstructured.Unstructured {
	return c.objs
}

func (c *components) Yaml() ([]byte, error) {
	return util.FromUnstructured(c.objs)
}

// newComponents returns a new objects embedding a component YAML file
//
// It is important to notice that clusterctl applies a set of processing steps to the “raw” component YAML read
// from the provider repositories:
// 1. Checks for all the variables in the component YAML file and replace with corresponding config values
// 2. Ensure all the provider components are deployed in the target namespace (apply only to namespaced objects)
// 3. Ensure all the ClusterRoleBinding which are referencing namespaced objects have the name prefixed with the namespace name
// 4. Set the watching namespace for the provider controller
// 5. Adds labels to all the components in order to allow easy identification of the provider objects
func newComponents(provider config.Provider, version string, rawyaml []byte, configVariablesClient config.VariablesClient, targetNamespace, watchingNamespace string) (*components, error) {

	// inspect the yaml read from the repository for variables
	variables := inspectVariables(rawyaml)

	// Replace variables with corresponding values read from the config
	yaml, err := replaceVariables(rawyaml, variables, configVariablesClient)
	if err != nil {
		return nil, errors.Wrap(err, "failed to perform variable substitution")
	}

	// transform the yaml in a list of objects, so following transformation can work on typed objects (instead of working on a string/slice of bytes)
	objs, err := util.ToUnstructured(yaml)
	if err != nil {
		return nil, errors.Wrap(err, "failed to parse yaml")
	}

	// inspect the list of objects for the default target namespace
	// the default target namespace is the namespace object defined in the component yaml read from the repository, if any
	defaultTargetNamespace, err := inspectTargetNamespace(objs)
	if err != nil {
		return nil, errors.Wrap(err, "failed to detect default target namespace")
	}

	// Ensures all the provider components are deployed in the target namespace (apply only to namespaced objects)
	// if targetNamespace is not specified, then defaultTargetNamespace is used. In case both targetNamespace and defaultTargetNamespace
	// are empty, an error is returned

	if targetNamespace == "" {
		targetNamespace = defaultTargetNamespace
	}

	if targetNamespace == "" {
		return nil, errors.New("target namespace can't be defaulted. Please specify a target namespace")
	}

	// add a Namespace object if missing (ensure the targetNamespace will be created)
	objs = addNamespaceIfMissing(objs, targetNamespace)

	// fix Namespace name in all the objects
	objs = fixTargetNamespace(objs, targetNamespace)

	// ensures all the ClusterRole and ClusterRoleBinding have the name prefixed with the namespace name and that
	// all the clusterRole/clusterRoleBinding namespaced subjects refers to targetNamespace
	// Nb. Making all the RBAC rules "namespaced" is required for supporting multi-tenancy
	objs, err = fixRBAC(objs, targetNamespace)
	if err != nil {
		return nil, errors.Wrap(err, "failed to fix ClusterRoleBinding names")
	}

	// inspect the list of objects for the default watching namespace
	// the default watching namespace is the namespace the controller is set for watching in the component yaml read from the repository, if any
	defaultWatchingNamespace, err := inspectWatchNamespace(objs)
	if err != nil {
		return nil, errors.Wrap(err, "failed to detect default watching namespace")
	}

	// if the requested watchingNamespace is different from the defaultWatchingNamespace, fix it
	if defaultWatchingNamespace != watchingNamespace {
		objs, err = fixWatchNamespace(objs, watchingNamespace)
		if err != nil {
			return nil, errors.Wrap(err, "failed to set watching namespace")
		}
	}

	objs = addLabels(objs, provider.Name())

	return &components{
		Provider:          provider,
		version:           version,
		variables:         variables,
		targetNamespace:   targetNamespace,
		watchingNamespace: watchingNamespace,
		objs:              objs,
	}, nil
}

func inspectVariables(data []byte) []string {
	variables := map[string]struct{}{}
	match := variableRegEx.FindAllStringSubmatch(string(data), -1)

	for _, m := range match {
		submatch := m[1]
		if _, ok := variables[submatch]; !ok {
			variables[submatch] = struct{}{}
		}
	}

	ret := make([]string, 0, len(variables))
	for v := range variables {
		ret = append(ret, v)
	}

	sort.Strings(ret)
	return ret
}

func replaceVariables(yaml []byte, variables []string, configVariablesClient config.VariablesClient) ([]byte, error) {
	tmp := string(yaml)
	var missingVariables []string
	for _, key := range variables {
		val, err := configVariablesClient.Get(key)
		if err != nil {
			missingVariables = append(missingVariables, key)
			continue
		}
		exp := regexp.MustCompile(`\$\{\s*` + key + `\s*\}`)
		tmp = exp.ReplaceAllString(tmp, val)
	}
	if len(missingVariables) > 0 {
		return nil, errors.Errorf("value for variables [%s] is not set. Please set the value using os environment variables or the clusterctl config file", strings.Join(missingVariables, ", "))
	}

	return []byte(tmp), nil
}

const namespaceKind = "Namespace"

// inspectTargetNamespace identifies the name of the namespace object contained in the components YAML, if any.
// In case more than one Namespace object is identified, an error is returned.
func inspectTargetNamespace(objs []unstructured.Unstructured) (string, error) {
	namespace := ""
	for _, o := range objs {
		// if the object has Kind Namespace
		if o.GetKind() == namespaceKind {
			// grab the name (or error if there is more than one Namespace object)
			if namespace != "" {
				return "", errors.New("Invalid manifest. There should be no more than one resource with Kind Namespace in the provider components yaml")
			}
			namespace = o.GetName()
		}
	}
	return namespace, nil
}

// addNamespaceIfMissing adda a Namespace object if missing (this ensure the targetNamespace will be created)
func addNamespaceIfMissing(objs []unstructured.Unstructured, targetNamespace string) []unstructured.Unstructured {
	namespaceObjectFound := false
	for _, o := range objs {
		// if the object has Kind Namespace, fix the namespace name
		if o.GetKind() == namespaceKind {
			namespaceObjectFound = true
		}
	}

	// if there isn't an object with Kind Namespace, add it
	if !namespaceObjectFound {
		objs = append(objs, unstructured.Unstructured{
			Object: map[string]interface{}{
				"kind": namespaceKind,
				"metadata": map[string]interface{}{
					"name": targetNamespace,
				},
			},
		})
	}

	return objs
}

// fixTargetNamespace ensures all the provider components are deployed in the target namespace (apply only to namespaced objects).
func fixTargetNamespace(objs []unstructured.Unstructured, targetNamespace string) []unstructured.Unstructured {
	for _, o := range objs {
		// if the object has Kind Namespace, fix the namespace name
		if o.GetKind() == namespaceKind {
			o.SetName(targetNamespace)
		}

		// if the object is namespaced, set the namespace name
		if isResourceNamespaced(o.GetKind()) {
			o.SetNamespace(targetNamespace)
		}
	}

	return objs
}

func isResourceNamespaced(kind string) bool {
	switch kind {
	case "Namespace",
		"Node",
		"PersistentVolume",
		"PodSecurityPolicy",
		"CertificateSigningRequest",
		"ClusterRoleBinding",
		"ClusterRole",
		"VolumeAttachment",
		"StorageClass",
		"CSIDriver",
		"CSINode",
		"ValidatingWebhookConfiguration",
		"MutatingWebhookConfiguration",
		"CustomResourceDefinition",
		"PriorityClass",
		"RuntimeClass":
		return false
	default:
		return true
	}
}

const (
	clusterRoleKind        = "ClusterRole"
	clusterRoleBindingKind = "ClusterRoleBinding"
	roleBindingKind        = "RoleBinding"
)

// fixRBAC ensures all the ClusterRole and ClusterRoleBinding have the name prefixed with the namespace name and that
// all the clusterRole/clusterRoleBinding namespaced subjects refers to targetNamespace
func fixRBAC(objs []unstructured.Unstructured, targetNamespace string) ([]unstructured.Unstructured, error) {

	renamedClusterRoles := map[string]string{}
	for _, o := range objs {
		// if the object has Kind ClusterRole
		if o.GetKind() == clusterRoleKind {
			// assign a namespaced name
			currentName := o.GetName()
			newName := fmt.Sprintf("%s-%s", targetNamespace, currentName)
			o.SetName(newName)

			renamedClusterRoles[currentName] = newName
		}
	}

	for i, o := range objs {
		switch o.GetKind() {
		case clusterRoleBindingKind: // if the object has Kind ClusterRoleBinding
			// Convert Unstructured into a typed object
			b := &rbacv1.ClusterRoleBinding{}
			if err := scheme.Scheme.Convert(&o, b, nil); err != nil { //nolint
				return nil, err
			}

			// assign a namespaced name
			b.Name = fmt.Sprintf("%s-%s", targetNamespace, b.Name)

			// ensure that namespaced subjects refers to targetNamespace
			for k := range b.Subjects {
				if b.Subjects[k].Namespace != "" {
					b.Subjects[k].Namespace = targetNamespace
				}
			}

			// if the referenced ClusterRole was renamed, change the RoleRef
			if newName, ok := renamedClusterRoles[b.RoleRef.Name]; ok {
				b.RoleRef.Name = newName
			}

			// Convert ClusterRoleBinding back to Unstructured
			if err := scheme.Scheme.Convert(b, &o, nil); err != nil { //nolint
				return nil, err
			}
			objs[i] = o

		case roleBindingKind: // if the object has Kind RoleBinding
			// Convert Unstructured into a typed object
			b := &rbacv1.RoleBinding{}
			if err := scheme.Scheme.Convert(&o, b, nil); err != nil { //nolint
				return nil, err
			}

			// ensure that namespaced subjects refers to targetNamespace
			for k := range b.Subjects {
				if b.Subjects[k].Namespace != "" {
					b.Subjects[k].Namespace = targetNamespace
				}
			}

			// Convert RoleBinding back to Unstructured
			if err := scheme.Scheme.Convert(b, &o, nil); err != nil { //nolint
				return nil, err
			}
			objs[i] = o
		}
	}

	return objs, nil
}

const namespaceArgPrefix = "--namespace="
const deploymentKind = "Deployment"
const controllerContainerName = "manager"

// inspectWatchNamespace inspects the list of components objects for the default watching namespace
// the default watching namespace is the namespace the controller is set for watching in the component yaml read from the repository, if any
func inspectWatchNamespace(objs []unstructured.Unstructured) (string, error) {
	namespace := ""
	// look for resources of kind Deployment
	for _, o := range objs {
		if o.GetKind() == deploymentKind {

			// Convert Unstructured into a typed object
			d := &appsv1.Deployment{}
			if err := scheme.Scheme.Convert(&o, d, nil); err != nil { //nolint
				return "", err
			}

			// look for a container with name "manager"
			for _, c := range d.Spec.Template.Spec.Containers {
				if c.Name == controllerContainerName {

					// look for the --namespace command arg
					for _, a := range c.Args {
						if strings.HasPrefix(a, namespaceArgPrefix) {
							n := strings.TrimPrefix(a, namespaceArgPrefix)
							if namespace != "" && n != namespace {
								return "", errors.New("Invalid manifest. All the controllers should watch have the same --namespace command arg in the provider components yaml")
							}
							namespace = n
						}
					}
				}
			}
		}
	}

	return namespace, nil
}

func fixWatchNamespace(objs []unstructured.Unstructured, watchingNamespace string) ([]unstructured.Unstructured, error) {

	// look for resources of kind Deployment
	for i, o := range objs {
		if o.GetKind() == deploymentKind {

			// Convert Unstructured into a typed object
			d := &appsv1.Deployment{}
			if err := scheme.Scheme.Convert(&o, d, nil); err != nil { //nolint
				return nil, err
			}

			// look for a container with name "manager"
			for j, c := range d.Spec.Template.Spec.Containers {
				if c.Name == controllerContainerName {

					// look for the --namespace command arg
					found := false
					for k, a := range c.Args {
						// if it exist
						if strings.HasPrefix(a, namespaceArgPrefix) {
							found = true

							// replace the command arg with the desired value or delete the arg if the controller should watch for objects in all the namespaces
							if watchingNamespace != "" {
								c.Args[k] = fmt.Sprintf("%s%s", namespaceArgPrefix, watchingNamespace)
								continue
							}
							c.Args = remove(c.Args, k)
						}
					}

					// if it not exists, and the controller should watch for objects in a specific namespace, set the command arg
					if !found && watchingNamespace != "" {
						c.Args = append(c.Args, fmt.Sprintf("%s%s", namespaceArgPrefix, watchingNamespace))
					}
				}

				d.Spec.Template.Spec.Containers[j] = c
			}

			// Convert Deployment back to Unstructured
			if err := scheme.Scheme.Convert(d, &o, nil); err != nil { //nolint
				return nil, err
			}
			objs[i] = o
		}
	}
	return objs, nil
}

func remove(slice []string, i int) []string {
	copy(slice[i:], slice[i+1:])
	return slice[:len(slice)-1]
}

// addLabels ensures all the provider components have a consistent set of labels
func addLabels(objs []unstructured.Unstructured, name string) []unstructured.Unstructured {
	for _, o := range objs {
		labels := o.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		for k, v := range getLabels(name) {
			labels[k] = v
		}
		o.SetLabels(labels)
	}

	return objs
}

func getLabels(name string) map[string]string {
	return map[string]string{
		clusterctlv1.ClusterctlLabelName:         "",
		clusterctlv1.ClusterctlProviderLabelName: name,
	}
}
