workflow {
  name          = "flat_l2"
  version       = "1"
  initial_state = "invoke_flat_l3"
  target_state  = "done"
}

subworkflow "flat_l3" {
  source = "./flat_l3"
}

step "invoke_flat_l3" {
  target = subworkflow.flat_l3
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
