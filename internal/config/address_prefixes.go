package config

// LocalAddressPrefixes returns every leading path segment a local agent
// address can carry: each rig name, plus each agent's Dir. A rig override can
// set Dir to something other than the rig name, so rig names alone miss
// addresses like "core/ray" for a rig "qcore" whose agents live under "core".
// The result is unordered and may contain duplicates.
func (c *City) LocalAddressPrefixes() []string {
	if c == nil {
		return nil
	}
	prefixes := make([]string, 0, len(c.Rigs)+len(c.Agents))
	for _, rig := range c.Rigs {
		prefixes = append(prefixes, rig.Name)
	}
	for i := range c.Agents {
		if dir := c.Agents[i].Dir; dir != "" {
			prefixes = append(prefixes, dir)
		}
	}
	return prefixes
}
