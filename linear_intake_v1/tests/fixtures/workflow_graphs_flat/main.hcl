workflow {
  name          = "workflow_graphs_flat"
  version       = "1"
  initial_state = "invoke_l1"
  target_state  = "done"
}

adapter "shell" "echo" {
  source  = "ghcr.io/brokenbots/criteria-adapter-shell"
  version = "0.5.3"
}

subworkflow "flat_l1" {
  source = "./flat_l1"
}

step "invoke_l1" {
  target = subworkflow.flat_l1
  outcome "success" { next = state.done }
  outcome "failure" { next = state.failed }
}

state "done" {
  terminal = true
  success  = true
}

state "failed" {
  terminal = true
  success  = false
}
