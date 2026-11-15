package main

// Harman's older Autobahn callee aborts on JSON null positional arguments.
// WAMP requires an array here when the following kwargs field is present.
func wampCallMessage(name string, args []any, kwargs M) []any {
	if args == nil {
		args = []any{}
	}
	if kwargs == nil {
		kwargs = M{}
	}
	return []any{48, 1, M{}, name, args, kwargs}
}
