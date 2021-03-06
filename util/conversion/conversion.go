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

package conversion

import (
	"context"
	"math/rand"
	"sort"
	"strings"
	"testing"

	fuzz "github.com/google/gofuzz"
	"github.com/onsi/gomega"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/apitesting/fuzzer"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metafuzzer "k8s.io/apimachinery/pkg/apis/meta/fuzzer"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/utils/diff"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1alpha3"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/conversion"
)

const (
	DataAnnotation = "cluster.x-k8s.io/conversion-data"
)

var (
	contract = clusterv1.GroupVersion.String()
)

// ConvertReferenceAPIContract takes a client and object reference, queries the API Server for
// the Custom Resource Definition and looks which one is the stored version available.
//
// The object passed as input is modified in place if an updated compatible version is found.
func ConvertReferenceAPIContract(ctx context.Context, c client.Client, ref *corev1.ObjectReference) error {
	gvk := ref.GroupVersionKind()
	crd, err := util.GetCRDWithContract(ctx, c, gvk, contract)
	if err != nil {
		return err
	}

	// If there is no label, return early without changing the reference.
	supportedVersions, ok := crd.Labels[contract]
	if !ok || supportedVersions == "" {
		return errors.Errorf("cannot find any versions matching contract %q for CRD %v", contract, crd.Name)
	}

	// Pick the latest version in the slice and validate it.
	kubeVersions := util.KubeAwareAPIVersions(strings.Split(supportedVersions, ","))
	sort.Sort(kubeVersions)
	chosen := kubeVersions[len(kubeVersions)-1]

	// Validate that the picked version is actually in the CRD spec.
	found := false
	for _, version := range crd.Spec.Versions {
		if version.Name == chosen {
			found = true
			break
		}
	}
	if !found {
		return errors.Errorf("cannot find any versions matching contract %q for CRD %v", contract, crd.Name)
	}

	// Modify the GroupVersionKind with the new version.
	if gvk.Version != chosen {
		gvk.Version = chosen
		ref.SetGroupVersionKind(gvk)
	}

	return nil
}

// MarshalData stores the source object as json data in the destination object annotations map.
func MarshalData(src metav1.Object, dst metav1.Object) error {
	data, err := json.Marshal(src)
	if err != nil {
		return err
	}
	if dst.GetAnnotations() == nil {
		dst.SetAnnotations(map[string]string{})
	}
	dst.GetAnnotations()[DataAnnotation] = string(data)
	return nil
}

// UnmarshalData tries to retrieve the data from the annotation and unmarshals it into the object passed as input.
func UnmarshalData(from metav1.Object, to interface{}) (bool, error) {
	data, ok := from.GetAnnotations()[DataAnnotation]
	if !ok {
		return false, nil
	}
	if err := json.Unmarshal([]byte(data), to); err != nil {
		return false, err
	}
	delete(from.GetAnnotations(), DataAnnotation)
	return true, nil
}

// GetFuzzer returns a new fuzzer to be used for testing.
func GetFuzzer(scheme *runtime.Scheme, funcs ...fuzzer.FuzzerFuncs) *fuzz.Fuzzer {
	funcs = append([]fuzzer.FuzzerFuncs{metafuzzer.Funcs}, funcs...)
	return fuzzer.FuzzerFor(
		fuzzer.MergeFuzzerFuncs(funcs...),
		rand.NewSource(rand.Int63()),
		serializer.NewCodecFactory(scheme),
	)
}

// FuzzTestFunc returns a new testing function to be used in tests to make sure conversions between
// the Hub version of an object and an older version aren't lossy.
func FuzzTestFunc(scheme *runtime.Scheme, hub conversion.Hub, dst conversion.Convertible, funcs ...fuzzer.FuzzerFuncs) func(*testing.T) {
	return func(t *testing.T) {
		g := gomega.NewWithT(t)
		fuzzer := GetFuzzer(scheme, funcs...)

		for i := 0; i < 10000; i++ {
			// Make copies of both objects, to avoid changing or re-using the ones passed in.
			hubCopy := hub.DeepCopyObject().(conversion.Hub)
			dstCopy := dst.DeepCopyObject().(conversion.Convertible)

			// Run the fuzzer on the Hub version copy.
			fuzzer.Fuzz(hubCopy)

			// Use the hub to convert into the convertible object.
			g.Expect(dstCopy.ConvertFrom(hubCopy)).To(gomega.Succeed())

			// Make another copy of hub and convert the convertible object back to the hub version.
			after := hub.DeepCopyObject().(conversion.Hub)
			g.Expect(dstCopy.ConvertTo(after)).To(gomega.Succeed())

			// Make sure that the hub before the conversions and after are the same, include a diff if not.
			g.Expect(apiequality.Semantic.DeepEqual(hubCopy, after)).To(gomega.BeTrue(), diff.ObjectDiff(hubCopy, after))
		}
	}
}
