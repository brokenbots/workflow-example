// KB-6: the admission gate predicate. Only a stamped url-type workflow
// without spec.workflowSource trips the gate; every legacy (non-url) and
// source-mode shape stays untouched.
package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
)

func TestWorkflowSourceMissing(t *testing.T) {
	urlWorkflow := &criteriav1.RunWorkflow{Name: "criteria-intake", Type: "url"}
	testcases := []struct {
		name string
		run  *criteriav1.CriteriaRun
		want bool
	}{
		{
			name: "workflow-less run keeps the legacy image-mode path",
			run:  &criteriav1.CriteriaRun{},
			want: false,
		},
		{
			name: "image-type workflow keeps the baked-tree path",
			run: &criteriav1.CriteriaRun{Spec: criteriav1.CriteriaRunSpec{
				Workflow: &criteriav1.RunWorkflow{Name: "baked-intake", Type: "image"},
			}},
			want: false,
		},
		{
			name: "url-type workflow with a declared workflowSource runs source mode",
			run: &criteriav1.CriteriaRun{Spec: criteriav1.CriteriaRunSpec{
				Workflow:       urlWorkflow.DeepCopy(),
				WorkflowSource: &criteriav1.RunWorkflowSource{Type: "url", URL: "git::https://example.com/wf.git"},
			}},
			want: false,
		},
		{
			name: "url-type workflow without a workflowSource trips the gate",
			run: &criteriav1.CriteriaRun{Spec: criteriav1.CriteriaRunSpec{
				Workflow: urlWorkflow.DeepCopy(),
			}},
			want: true,
		},
		{
			name: "an unrecognized workflow type is not the url gate's business",
			run: &criteriav1.CriteriaRun{Spec: criteriav1.CriteriaRunSpec{
				Workflow: &criteriav1.RunWorkflow{Name: "unvalidated", Type: "git"},
			}},
			want: false,
		},
	}
	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, workflowSourceMissing(tc.run))
		})
	}
}