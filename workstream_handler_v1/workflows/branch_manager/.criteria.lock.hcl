schema_version = 1
adapter "copilot" "branch_repair" {
  reference            = "ghcr.io/brokenbots/criteria-adapter-copilot:0.5.15"
  version              = "0.5.15"
  resolved_digest      = "sha256:827e39ad040a24ad81251b802b1996a00195121ce2c97b7b6ba62d558d581773"
  source_url           = "https://github.com/brokenbots/criteria-adapter-copilot"
  sdk_protocol_version = 2
  platforms            = ["linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"]
  signature {
    keyless {
      issuer  = "https://token.actions.githubusercontent.com"
      subject = "https://github.com/brokenbots/criteria-adapter-copilot/.github/workflows/publish.yml@refs/tags/v0.5.15"
    }
  }
}
adapter "shell" "repo" {
  reference            = "ghcr.io/brokenbots/criteria-adapter-shell:0.5.4"
  version              = "0.5.4"
  resolved_digest      = "sha256:2530b8251d00c44e714418ae2b59b9237e1d673cb81ddb81eb54eb37ee00ef11"
  source_url           = "https://github.com/brokenbots/criteria-adapter-shell"
  sdk_protocol_version = 2
  platforms            = ["linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"]
  signature {
    keyless {
      issuer  = "https://token.actions.githubusercontent.com"
      subject = "https://github.com/brokenbots/criteria-adapter-shell/.github/workflows/publish.yml@refs/tags/v0.5.4"
    }
  }
}
adapter "shell" "sh" {
  reference            = "ghcr.io/brokenbots/criteria-adapter-shell:0.5.4"
  version              = "0.5.4"
  resolved_digest      = "sha256:2530b8251d00c44e714418ae2b59b9237e1d673cb81ddb81eb54eb37ee00ef11"
  source_url           = "https://github.com/brokenbots/criteria-adapter-shell"
  sdk_protocol_version = 2
  platforms            = ["linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"]
  signature {
    keyless {
      issuer  = "https://token.actions.githubusercontent.com"
      subject = "https://github.com/brokenbots/criteria-adapter-shell/.github/workflows/publish.yml@refs/tags/v0.5.4"
    }
  }
}
