// Package sched fires recurring jobs into the one job queue. Schedules live
// in SQLite and are rebuilt from it on every Start, so a restart loses no
// schedule and gains no side channel: every fire goes through
// (*queue.Queue).Enqueue like a keypress in the TUI or a webhook rule would.
package sched
