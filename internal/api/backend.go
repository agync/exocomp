package api

import "fmt"

// BackendBlock is the ready-to-paste OpenTofu `backend "http"` configuration
// for one workspace's state. Both the CLI (`workspace add --state-only`) and
// the web surface build it through NewBackendBlock, so the address and bearer
// a user pastes are always computed the one way: StateURL and WorkspaceToken.
type BackendBlock struct {
	Workspace string
	Address   string
	Token     string
}

// NewBackendBlock computes the block for workspace at the state backend
// bound to addr.
func NewBackendBlock(d TokenDeriver, addr, workspace string) BackendBlock {
	return BackendBlock{
		Workspace: workspace,
		Address:   StateURL(addr, workspace),
		Token:     WorkspaceToken(d, workspace),
	}
}

// HCL renders the terraform block the operator pastes into a root module.
// It deliberately carries NO credential: the http backend's headers are sent
// verbatim by tofu (no ${var...} interpolation), so a pasted bearer would be
// a secret on screen. The value arrives at init via -backend-config=<(
// exocomp workspace backend <name>) — see BackendConfig. lock_address and
// unlock_address repeat Address: the SQLite backend serves LOCK/UNLOCK on the
// state path.
func (b BackendBlock) HCL() string {
	return fmt.Sprintf(`terraform {
  backend "http" {
    address        = %[1]q
    lock_address   = %[1]q
    unlock_address = %[1]q
    lock_method    = "LOCK"
    unlock_method  = "UNLOCK"
  }
}
`, b.Address)
}

// BackendConfig renders the partial backend configuration `tofu init
// -backend-config=` consumes: the address block plus the literal bearer in a
// headers map, exactly the shape the executor hands tofu (internal/executor/
// tofu.go writeBackendConfig). It is the value behind the reference block's
// init line, and the bearer never appears in a paste block — only in this
// filled form, which the CLI emits to a process pipe or a 0600 private file.
func (b BackendBlock) BackendConfig() string {
	return fmt.Sprintf(`address        = %[1]q
lock_address   = %[1]q
unlock_address = %[1]q
lock_method    = "LOCK"
unlock_method  = "UNLOCK"
headers = {
  Authorization = "Bearer %[2]s"
}
`, b.Address, b.Token)
}
