package store

// ScheduleTofuPlan is drift detection as a schedule: a `tofu plan` whose exit
// code 2 becomes a reactive event, so an apply can be gated behind the same
// approval as any other reactive action.
const ScheduleTofuPlan = "tofu-plan"

// TofuWorkspace reports the workspace a tofu-plan schedule plans. It reads the
// args column's target field, which names the one thing a fire acts on
// whatever the kind: a backup target for kind backup, a workspace here.
func (a ScheduleArgs) TofuWorkspace() string { return a.Target }
