package store

// ScheduleDrill is a recurring restore drill: fire RestoreDrill against a
// target (or every enabled target, when Args.Target is empty) on a cadence,
// so a silently broken envelope fails a routine job instead of being
// discovered during a real disaster
// (docs/operations/disaster-recovery.md's monthly dr-drill).
const ScheduleDrill = "drill"
