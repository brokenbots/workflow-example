workflow {
  name          = "flat_l3"
  version       = "1"
  initial_state = "echo"
  target_state  = "done"
}

adapter "shell" "echo" {
  source  = "ghcr.io/brokenbots/criteria-adapter-shell"
  version = "0.5.3"
}

step "echo" {
  target = adapter.shell.echo
  input {
    command = "echo flat_l3 reached"
  }
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
