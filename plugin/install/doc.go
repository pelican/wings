// Package install adds and updates external plugins: it resolves a name in
// the configured registries, downloads the archive over https, checks its
// sha256 and signature, unpacks it into a staging directory under an os.Root,
// and swaps it into place with a backup so the swap can be rolled back.
//
// Nothing here runs a plugin. Callers stage with StageAdd or StageUpdate (a
// registry or update manifest), or StageURL or StageFile (any archive), show
// the staged manifest, and pass the stage to Install with an Approver. Install
// commits, records the approval in the trust store, and rolls back if that
// fails. `wings plugins add` and `update` (cmd/plugins.go) approve at a
// prompt; the Panel API (router/router_plugins.go) approves with a token.
package install
