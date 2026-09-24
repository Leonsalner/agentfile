package state

import "io/fs"

// restrictToOwner relies on %LOCALAPPDATA%'s inherited per-user ACL; agentfile
// does not rewrite Windows ACLs. See README "Windows limitations".
func restrictToOwner(string, fs.FileInfo) error { return nil }
