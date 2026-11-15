// Package install adds and updates external plugins: it resolves a name in
// the configured registries, downloads the archive over https, checks its
// sha256 and signature, unpacks it into a staging directory under an os.Root,
// and swaps it into place with a backup so the swap can be rolled back.
//
// Nothing here runs a plugin or records the operator's approval. Callers stage
// with StageAdd or StageUpdate, show the staged manifest, Commit, record the
// approval in the trust store (plugin.LoadTrustStore), then Finish, or
// Rollback if recording the approval failed. `wings plugins add` and `update`
// are the callers (cmd/plugins.go).
package install
