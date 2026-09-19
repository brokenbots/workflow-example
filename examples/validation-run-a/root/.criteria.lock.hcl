schema_version = 1
adapter "shell" "ack" {
  reference            = "ghcr.io/brokenbots/criteria-adapter-shell:0.5.3"
  version              = "0.5.3"
  resolved_digest      = "sha256:d9f306c29f4145da8bcc44187c9e4ae0f69ed30db3b3edac6e9b6350469bc635"
  source_url           = "https://github.com/brokenbots/criteria-adapter-shell"
  sdk_protocol_version = 2
  platforms            = ["linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"]
  signature {
    keyless {
      issuer  = "https://token.actions.githubusercontent.com"
      subject = "https://github.com/brokenbots/criteria-adapter-shell/.github/workflows/publish.yml@refs/tags/v0.5.3"
    }
  }
}
workflow_ref "child" {
  source       = "git::https://github.com/brokenbots/workflow-example.git//examples/validation-run-a/child?ref=4e0fd3dca73148b3d9fa438c983d84ea46514f1b"
  resolved_ref = "4e0fd3dca73148b3d9fa438c983d84ea46514f1b"
  kind         = "git"
}
