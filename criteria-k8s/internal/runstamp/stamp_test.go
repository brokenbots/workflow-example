package runstamp

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/routes"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := criteriav1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func TestStampRepoConfigNilSafe(t *testing.T) {
	var spec criteriav1.CriteriaRunSpec
	t.Run("nil selection is a no-op", func(t *testing.T) {
		spec = criteriav1.CriteriaRunSpec{}
		StampRepoConfig(&spec, nil, &criteriav1.CriteriaRunConfigRef{Name: "x"})
		if spec.ConfigRef != nil {
			t.Fatalf("nil selection stamped ConfigRef %+v", spec.ConfigRef)
		}
	})
	t.Run("entry values outrank the spec fields entry-by-entry", func(t *testing.T) {
		spec = criteriav1.CriteriaRunSpec{
			BuildCmd:  "flag-build",
			TestCmd:   "flag-test",
			CIGateCmd: "flag-gate",
		}
		StampRepoConfig(&spec, &routes.Selection{
			ConfigName: "repo-a",
			Config:     routes.ConfigEntry{BuildCmd: "make build-a"},
		}, &criteriav1.CriteriaRunConfigRef{Name: "repo-a", ResourceVersion: "42"})
		if spec.BuildCmd != "make build-a" {
			t.Errorf("BuildCmd = %q, want entry value", spec.BuildCmd)
		}
		if spec.TestCmd != "flag-test" || spec.CIGateCmd != "flag-gate" {
			t.Errorf("keys the entry omits must keep the flag values: TestCmd %q, CIGateCmd %q", spec.TestCmd, spec.CIGateCmd)
		}
		if spec.ConfigRef == nil || spec.ConfigRef.Name != "repo-a" || spec.ConfigRef.ResourceVersion != "42" {
			t.Fatalf("ConfigRef = %+v, want the pinned ref", spec.ConfigRef)
		}
	})
	t.Run("nil ref clears any prior pin", func(t *testing.T) {
		spec = criteriav1.CriteriaRunSpec{}
		spec.ConfigRef = &criteriav1.CriteriaRunConfigRef{Name: "stale"}
		StampRepoConfig(&spec, &routes.Selection{}, nil)
		if spec.ConfigRef != nil {
			t.Fatalf("no config selected: ConfigRef = %+v, want nil", spec.ConfigRef)
		}
	})
}

func TestPinConfigRef(t *testing.T) {
	scheme := testScheme(t)
	ctx := context.Background()
	ns := "criteria-jobs"

	setup := func(t *testing.T, objs ...client.Object) client.Client {
		t.Helper()
		return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	}

	t.Run("nil or config-less selection stamps no pin", func(t *testing.T) {
		c := setup(t)
		for _, sel := range []*routes.Selection{nil, {ConfigName: ""}} {
			ref, err := PinConfigRef(ctx, c, ns, sel)
			if err != nil || ref != nil {
				t.Fatalf("PinConfigRef(%+v) = %+v, %v; want nil, nil", sel, ref, err)
			}
		}
	})

	t.Run("existing CM pins name and resourceVersion", func(t *testing.T) {
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "repo-a", Namespace: ns, ResourceVersion: "77"},
		}
		c := setup(t, cm)
		ref, err := PinConfigRef(ctx, c, ns, &routes.Selection{ConfigName: "repo-a"})
		if err != nil {
			t.Fatalf("PinConfigRef: %v", err)
		}
		if ref.Name != "repo-a" || ref.ResourceVersion != "77" {
			t.Fatalf("ref = %+v, want {repo-a 77}", ref)
		}
	})

	t.Run("absent CM yields a name-only pin", func(t *testing.T) {
		c := setup(t)
		ref, err := PinConfigRef(ctx, c, ns, &routes.Selection{ConfigName: "repo-a"})
		if err != nil {
			t.Fatalf("PinConfigRef: %v", err)
		}
		if ref.Name != "repo-a" || ref.ResourceVersion != "" {
			t.Fatalf("ref = %+v, want name-only {repo-a}", ref)
		}
	})

	t.Run("other read errors propagate", func(t *testing.T) {
		want := apierrors.NewInternalError(errors.New("etcd down"))
		c := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
			Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
				return want
			},
		}).Build()
		ref, err := PinConfigRef(ctx, c, ns, &routes.Selection{ConfigName: "repo-a"})
		if !errors.Is(err, want) {
			t.Fatalf("err = %v, want the propagated internal error", err)
		}
		if ref.Name != "repo-a" {
			t.Fatalf("ref = %+v, want the name retained", ref)
		}
	})
}