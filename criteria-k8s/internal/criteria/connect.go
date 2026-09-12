package criteria

import (
	"net/http"

	connect "connectrpc.com/connect"

	"github.com/brokenbots/workflow-example/criteria-k8s/internal/criteria/pb/criteria/v1/criteriav1connect"
)

// ServiceClient is the Connect client interface for the CriteriaService.
// Use [NewServiceClient] to construct an implementation.
type ServiceClient = criteriav1connect.CriteriaServiceClient

// ServiceHandler is the server-side handler interface for the CriteriaService.
// Orchestrators implement this interface to receive calls from a criteria agent.
type ServiceHandler = criteriav1connect.CriteriaServiceHandler

// CriteriaServiceClient is an alias for [ServiceClient].
type CriteriaServiceClient = ServiceClient

// CriteriaServiceHandler is an alias for [ServiceHandler].
type CriteriaServiceHandler = ServiceHandler

// NewServiceClient constructs a [ServiceClient] that speaks to baseURL.
// By default it uses the Connect protocol with binary Protobuf encoding.
// Pass connect.WithGRPC() or connect.WithGRPCWeb() to use those protocols.
var NewServiceClient = criteriav1connect.NewCriteriaServiceClient

// NewServiceHandler builds an HTTP handler from a [ServiceHandler] implementation.
// It returns the URL path prefix and the handler itself, ready to mount on an
// http.ServeMux. The handler supports Connect, gRPC, and gRPC-Web protocols.
func NewServiceHandler(svc ServiceHandler, opts ...connect.HandlerOption) (string, http.Handler) {
	return criteriav1connect.NewCriteriaServiceHandler(svc, opts...)
}

// Service name and procedure path constants forwarded from the generated package.
const (
	ServiceName = criteriav1connect.CriteriaServiceName

	RegisterProcedure     = criteriav1connect.CriteriaServiceRegisterProcedure
	HeartbeatProcedure    = criteriav1connect.CriteriaServiceHeartbeatProcedure
	CreateRunProcedure    = criteriav1connect.CriteriaServiceCreateRunProcedure
	ReattachRunProcedure  = criteriav1connect.CriteriaServiceReattachRunProcedure
	ResumeProcedure       = criteriav1connect.CriteriaServiceResumeProcedure
	SubmitEventsProcedure = criteriav1connect.CriteriaServiceSubmitEventsProcedure
	ControlProcedure      = criteriav1connect.CriteriaServiceControlProcedure
)
