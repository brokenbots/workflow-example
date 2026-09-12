// Package criteria is the criteria→orchestrator wire contract SDK.
//
// # Vendored copy
//
// This tree is a vendored copy of github.com/brokenbots/criteria/sdk carrying
// the CRI-131 wire extensions (CreateRunRequest.ticket/repo_url and
// Run.ticket/repo_url/pr_url) that upstream has not released yet. Replace it
// with a published SDK version once one carries those fields.
//
// # Overview
//
// This package is the single boundary surface between a criteria agent execution
// engine and any orchestrator that stores and distributes run events. It
// re-exports — without wrapping or transforming — the generated Connect/gRPC
// service stubs, envelope and payload types, and a small set of helper
// functions that every implementation needs.
//
// Module path: github.com/brokenbots/workflow-example/criteria-k8s/internal/criteria
//
// # What this package exports
//
//   - [ServiceClient] / [ServiceHandler] — Connect interface aliases for the
//     generated CriteriaService stubs. Use [NewServiceClient] to construct a
//     client; implement [ServiceHandler] to expose the service.
//     [CriteriaServiceClient] and [CriteriaServiceHandler] are migration-
//     compatibility aliases for the same types; prefer the Service* forms for
//     new code.
//   - Envelope and payload type aliases for every event shape defined in
//     proto/criteria/v1/events.proto.
//   - [NewEnvelope], [TypeString], [IsTerminal] — event helpers.
//   - [SchemaVersion] — the current event protocol version constant.
//
// # What this package does NOT export
//
// The SDK is deliberately minimal. None of the following cross the boundary:
//
//   - Storage interfaces (sql.DB, store.Store, or any orchestrator-specific
//     persistence layer).
//   - In-memory fan-out / control-message routing components.
//   - Run-state machine helpers (the orchestrator decides what "paused" means
//     from the semantic events it receives; the execution engine does not
//     prescribe a state model).
//   - Any type or symbol defined under internal/ or workflow/.
//
// # Type alias vs wrapper function rule
//
// Types are re-exported as Go type aliases (type Foo = pb.Foo). Aliases
// preserve assignability: code that already holds a *pb.RunStarted can pass
// it as an *criteria.RunStarted without conversion.
//
// Functions are re-exported as wrapper functions (func F(...) { return impl.F(...) }).
// A wrapper adds one stack frame; this is acceptable at the SDK boundary.
//
// # Auth contract
//
// All RPCs except [ServiceHandler.Register] require a bearer token in one of:
//   - HTTP Authorization header (Bearer scheme)
//   - X-Criteria-Token header
//   - criteria-token Connect metadata key
//
// Tokens are SHA-256 compared against the orchestrator's stored token hash.
// Implementations MUST enforce caller-ownership on every mutating RPC: the
// authenticated caller's criteria ID must own the agent or run being
// mutated. Register is bootstrap-only; implementations MUST gate it behind a
// deployment-defined bootstrap credential (e.g. a pre-shared secret in a
// deployment-defined header) or return Unimplemented when no bootstrap
// mechanism is configured.
//
// # Schema version semantics
//
// [SchemaVersion] = 1 is the v0.1 SDK. The value matches the proto package
// major version (criteria.v1). A bump to SchemaVersion 2 introduces a new
// proto package (criteria.v2) and a new SDK minor release; both sides must
// coordinate. Until a real versioning need arises, schema negotiation helpers
// are out of scope.
package criteria
