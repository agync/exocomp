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

// HCL renders the terraform block itself. lock_address and unlock_address
// repeat Address: the SQLite backend serves LOCK/UNLOCK on the state path.
func (b BackendBlock) HCL() string {
	return fmt.Sprintf(`terraform {
  backend "http" {
    address        = %[1]q
    lock_address   = %[1]q
    unlock_address = %[1]q
    lock_method    = "LOCK"
    unlock_method  = "UNLOCK"
    headers = {
      Authorization = "Bearer %[2]s"
    }
  }
}
`, b.Address, b.Token)
}
