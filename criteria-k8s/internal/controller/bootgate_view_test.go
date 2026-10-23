package controller

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestBootgateViewJobFirstRegression covers the KB-301 false-park shadow:
// the ticket family holds a NEWER terminal Failed row (the rapid re-arm
// interleaving) while a different-class CR boots. The request carries both
// ticket and job (as the runner does), and the booting CR's own identity
// must win the verdict — never the family's terminal row. It also covers
// the resolution paths of the job branch: the job-label exact match (and
// its namespacing — a matching CR in another namespace must not leak in)
// and the CR-name match (watcher-shaped CRs carry no job label, so the job
// param is the CR name), plus the preserved family fallback and the
// KB-234 orphan close (job unresolved → NotFound, never a family verdict).
func TestBootgateViewJobFirstRegression(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := criteriav1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	early, late := metav1.NewTime(time.Unix(1, 0)), metav1.NewTime(time.Unix(2, 0))
	booting := &criteriav1.CriteriaRun{ObjectMeta: metav1.ObjectMeta{
		Name: "kb-42-1000", Namespace: "ns",
		// Realistic watcher stamping: the ticket label only — no job label.
		Labels: map[string]string{"ticket": "kb-42"},
		CreationTimestamp: early},
		Status: criteriav1.CriteriaRunStatus{Phase: "Pending"}}
	// The shadow: a NEWER, different-class (higher unix suffix) family row
	// parked in a terminal Failed phase — both the newer timestamp and the
	// larger suffix defeat the family branch's newest-wins on every stale
	// shape of that selection.
	shadow := &criteriav1.CriteriaRun{ObjectMeta: metav1.ObjectMeta{
		Name: "kb-42-2000", Namespace: "ns",
		Labels: map[string]string{"ticket": "kb-42"},
		CreationTimestamp: late},
		Status: criteriav1.CriteriaRunStatus{Phase: "Failed"}}
	labeledNS := &criteriav1.CriteriaRun{ObjectMeta: metav1.ObjectMeta{
		// The job label's path: the CR name differs from the job param.
		Name: "cri-77", Namespace: "ns",
		Labels: map[string]string{"criteria.brokenbots.dev/job": "cri-77-runner"},
		CreationTimestamp: early},
		Status: criteriav1.CriteriaRunStatus{Phase: "Running"}}
	// Decoy: same job label, same name suffix trick, but in the namespace
	// adjacent to "ns" — a cluster-scoped List (the pre-fix RBAC bug) would
	// resolve it (decoy = newer terminal Failed) instead of the namespaced
	// authoritative row.
	decoy := &criteriav1.CriteriaRun{ObjectMeta: metav1.ObjectMeta{
		Name: "cri-77", Namespace: "other",
		Labels: map[string]string{"criteria.brokenbots.dev/job": "cri-77-runner"},
		CreationTimestamp: late},
		Status: criteriav1.CriteriaRunStatus{Phase: "Failed"}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(booting, shadow, labeledNS, decoy).Build()
	v := &BootgateView{Client: cl, Namespace: "ns"}

	for _, tc := range []struct {
		name, url, wantPhase string
		wantTerminal         bool
	}{{
		// THE incident shape: newer terminal Failed row in the family, the
		// booting CR's phase must be the verdict, not the shadow's.
		name: "job identity beats a newer terminal family row",
		url:  "/operator-view?ticket=KB-42&job=kb-42-1000&ns=ns", wantPhase: "Pending",
	}, {
		// The job-label path resolves even when the CR name differs from
		// the job, and the List is namespaced: the same-label decoy in
		// another namespace (newer, Failed) must not leak in.
		name: "job-label resolution is namespaced", url: "/operator-view?job=cri-77-runner&ns=ns", wantPhase: "Running",
	}, {
		// No job: the family fallback keeps its legacy verdict shape
		// (newest family row = the newer shadow here).
		name: "jobless request falls back to the family newest",
		url:  "/operator-view?ticket=KB-42&ns=ns", wantPhase: "Failed", wantTerminal: true,
	}, {
		// KB-234 orphan close preserved: a job the view cannot resolve to a
		// CR answers NotFound (never a sibling's phase — family is not
		// consulted on a job miss).
		name: "unresolvable job stays NotFound",
		url:  "/operator-view?ticket=KB-42&job=kb-42-gone&ns=ns", wantPhase: "NotFound", wantTerminal: true,
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

func TestBootgateViewVerdicts(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := criteriav1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	live := &criteriav1.CriteriaRun{ObjectMeta: metav1.ObjectMeta{Name: "kb-42-100", Namespace: "ns", Labels: map[string]string{"ticket": "kb-42"}},
		Status: criteriav1.CriteriaRunStatus{Phase: "Running"}}
	old := &criteriav1.CriteriaRun{ObjectMeta: metav1.ObjectMeta{Name: "kb-42-99", Namespace: "ns", Labels: map[string]string{"ticket": "kb-42"}},
		Status: criteriav1.CriteriaRunStatus{Phase: "Failed"}}
	dead := &criteriav1.CriteriaRun{ObjectMeta: metav1.ObjectMeta{Name: "kb-43-1", Namespace: "ns", Labels: map[string]string{"ticket": "kb-43"}},
		Status: criteriav1.CriteriaRunStatus{Phase: "Failed"}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(live, dead, old).Build()
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
