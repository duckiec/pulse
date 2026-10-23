package app

import (
	"pulse/internal/engine"
	"pulse/internal/mcp"
	"pulse/internal/secrets"
)

// NewRecorder builds the traffic recorder behind `pulse record`.
//
// It deliberately does NOT boot the whole application: recording is a local
// proxy, and requiring a keychain probe, a sync network and a RAG index to
// capture traffic would make the zero-friction command the one that fails on a
// fresh machine. What it does take from the configuration is the ephemeral
// .env: those values are registered with a redactor so a token passed in a
// query string or a body is scrubbed from the generated collection, not just
// from credential headers. A missing or unreadable .env is reported and
// ignored; header redaction still works without it.
func NewRecorder(cfg Config, target, out string) (*engine.Recorder, error) {
	red := secrets.NewRedactor()
	if cfg.EnvFile != "" {
		env, err := secrets.LoadEnvFile(cfg.EnvFile)
		if err != nil {
			mcp.Logf("record: the ephemeral env at %s could not be read; "+
				"only header-name redaction is active: %v", cfg.EnvFile, err)
		} else {
			for _, v := range env {
				red.Register(v)
			}
		}
	}
	return engine.NewRecorder(engine.RecordConfig{
		Target:   target,
		Out:      out,
		Redactor: red,
		Logf:     mcp.Logf,
	})
}
