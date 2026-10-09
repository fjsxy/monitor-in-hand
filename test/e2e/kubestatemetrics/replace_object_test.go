package kubestatemetrics_test

import (
	"encoding/json"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestReplaceObjectManifest(t *testing.T) {
	t.Run("UT-KSM-GH82-003 omits server-owned status fields", func(t *testing.T) {
		replicas := int32(2)
		statefulSet := appsv1.StatefulSet{
			TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "StatefulSet"},
			ObjectMeta: metav1.ObjectMeta{
				Name:            "ksm-gh82-statefulset-sharded",
				ResourceVersion: "123",
			},
			Spec: appsv1.StatefulSetSpec{Replicas: &replicas},
			Status: appsv1.StatefulSetStatus{
				AvailableReplicas: 2,
			},
		}

		manifest, err := replaceObjectManifest(&statefulSet)
		if err != nil {
			t.Fatalf("replaceObjectManifest() error = %v", err)
		}

		var object map[string]json.RawMessage
		if err := json.Unmarshal(manifest, &object); err != nil {
			t.Fatalf("unmarshal replace manifest: %v", err)
		}
		if status, found := object["status"]; found {
			t.Fatalf("replace manifest contains server-owned status %s", status)
		}

		var replaced appsv1.StatefulSet
		if err := json.Unmarshal(manifest, &replaced); err != nil {
			t.Fatalf("unmarshal typed replace manifest: %v", err)
		}
		if replaced.APIVersion != "apps/v1" || replaced.Kind != "StatefulSet" ||
			replaced.Name != statefulSet.Name || replaced.ResourceVersion != statefulSet.ResourceVersion ||
			replaced.Spec.Replicas == nil || *replaced.Spec.Replicas != replicas {
			t.Fatalf("replace manifest lost writable identity or spec: %#v", replaced)
		}
	})
}
