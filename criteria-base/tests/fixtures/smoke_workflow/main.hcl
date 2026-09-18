workflow {
  name          = "smoke_trivial"
  version       = "1"
  initial_state = "run"
  target_state  = "done"
}

adapter "shell" "echo" {
  source  = "ghcr.io/brokenbots/criteria-adapter-shell"
  version = "0.5.3"
  config {}
}

step "run" {
  target = adapter.shell.echo
  input {
    command = "echo criteria-base smoke ok"
  }
  outcome "success" { next = state.done }
  outcome "failure" { next = state.failed }
}

state "done" {
  terminal = true
}

state "failed" {
  terminal = true
  success  = false
}
