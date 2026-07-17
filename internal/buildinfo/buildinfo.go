package buildinfo

// Version tracks the compatibility version exposed by the Python oracle.
// Release automation must keep both values equal while the runtimes coexist.
const Version = "1.9.91"

// Implementation identifies the runtime in logs, metrics, and usage events.
const Implementation = "go"
