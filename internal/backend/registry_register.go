package backend

// Register adds a backend to the registry under the given name.
// This is useful for programmatic registration, particularly in tests.
func (r *Registry) Register(name string, b Backend) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.backends[name] = b
}
