package v2

// Launchd and Task Scheduler already restart a process that exits 2, and
// "crash" on Windows would raise an exception into Windows Error
// Reporting rather than exit, so a Windows trial keeps Go's default.
func crashNotExit()     {}
func restoreTraceback() {}
