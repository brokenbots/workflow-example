package controller

import (
	"net/http/httptest"
	"strings"
	"testing"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestBootgateViewVerdicts(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := criteriav1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	live := &criteriav1.CriteriaRun{ObjectMeta: metav1.ObjectMeta{Name: "kb-42", Namespace: "ns"},
		Status: criteriav1.CriteriaRunStatus{Phase: "Running"}}
	dead := &criteriav1.CriteriaRun{ObjectMeta: metav1.ObjectMeta{Name: "kb-43", Namespace: "ns"},
		Status: criteriav1.CriteriaRunStatus{Phase: "Failed"}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(live, dead).Build()
	v := &BootgateView{Client: cl, Namespace: "ns"}

	for _, tc := range []struct {
		name, url, wantPhase string
		wantTerminal         bool
	}{{
		name: "live run reports Running open", url: "/operator-view?ticket=KB-42&ns=ns", wantPhase: "Running",

	}, {
		name: "missing ns defaults to the operator namespace", url: "/operator-view?ticket=KB-42", wantPhase: "Running",
	}, {
		name: "terminal run reports Failed terminal", url: "/operator-view?ticket=KB-43&ns=ns", wantPhase: "Failed", wantTerminal: true,
	}, {
		name: "deleted CR reports NotFound terminal", url: "/operator-view?ticket=KB-99&ns=ns", wantPhase: "NotFound", wantTerminal: true,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			v.ServeHTTP(w, httptest.NewRequest("GET", tc.url, nil))
			if w.Code != 200 {
				t.Fatalf("code %d body %s", w.Code, w.Body.String())
			}
			body := w.Body.String()
			if want := `"phase":"` + tc.wantPhase + `"`; !strings.Contains(body, want) {
				t.Fatalf("body %s missing %q", body, want)
			}
			if tc.wantTerminal && !strings.Contains(body, `"terminal":true`) {
				t.Fatalf("body %s missing terminal:true", body)
			}
			if !tc.wantTerminal && !strings.Contains(body, `"terminal":false`) {
				t.Fatalf("body %s missing terminal:false", body)
			}
		})
	}
}