workflow {
  name          = "flat_l1"
  version       = "1"
  initial_state = "invoke_flat_l2"
  target_state  = "done"
}

subworkflow "flat_l2" {
  source = "./flat_l2"
}

step "invoke_flat_l2" {
  target = subworkflow.flat_l2
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
