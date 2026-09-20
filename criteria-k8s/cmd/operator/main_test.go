// CRI-264: the wiring regression the controller suite cannot see. The
// shipped operator's scheme is built in package main, and a typed read of
// an unregistered kind fails before any API call ("no kind is registered
// for the type ..."). The missing apps/v1 registration made the
// DeploymentEnvProbe's Deployment read — and with it the base-image
// admission-race enforcement — dead in the shipped binary while every
// controller test (stub probe, test-local scheme with appsv1) stayed
// green. These tests exercise the production scheme itself: removing the
// appsv1 registration in operatorScheme() fails both of them.
package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/brokenbots/workflow-example/criteria-k8s/internal/controller"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/jobbuilder"
)

// The production scheme must resolve every kind the operator's clients Get
// by value: DeploymentEnvProbe reads a typed *appsv1.Deployment, which
// requires the apps/v1 Deployment GVK in the scheme.
func TestOperatorSchemeResolvesDeploymentKind(t *testing.T) {
	scheme, err := operatorScheme()
	require.NoError(t, err)

	kinds, unversioned, err := scheme.ObjectKinds(&appsv1.Deployment{})
	require.NoError(t, err, "the production scheme must register the apps/v1 Deployment kind")
	require.False(t, unversioned, "Deployment is a versioned apps/v1 type, not an unversioned one")
	assert.Contains(t, kinds, appsv1.SchemeGroupVersion.WithKind("Deployment"))
}

// The probe the binary deploys must read the live operator Deployment
// through the production scheme: the base-image resolution, the
// BaseImageMismatch fail-fast, and the status.baseImage stamp all depend on
// this read succeeding (the controller suite's own probe test builds a
// scheme that always contains apps/v1, so it cannot catch a missing
// registration here).
func TestDeploymentEnvProbeOverProductionScheme(t *testing.T) {
	scheme, err := operatorScheme()
	require.NoError(t, err)

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "criteria-k8s-operator", Namespace: "criteria-jobs"},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "operator",
							Env: []corev1.EnvVar{
								{Name: jobbuilder.EnvCriteriaBaseImage, Value: "localhost:5000/criteria-base:v27"},
								{Name: "OTHER", Value: "x"},
							},
						},
					},
				},
			},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy).Build()
	probe := &controller.DeploymentEnvProbe{Client: cl, Namespace: "criteria-jobs", Deployment: "criteria-k8s-operator"}

	env, err := probe.OperatorEnv(context.Background(), jobbuilder.EnvCriteriaBaseImage, jobbuilder.EnvDefaultImage)
	require.NoError(t, err, "the production scheme must serve the operator-env Deployment read")
	assert.Equal(t, map[string]string{jobbuilder.EnvCriteriaBaseImage: "localhost:5000/criteria-base:v27"}, env)
}