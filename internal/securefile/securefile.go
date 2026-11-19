// Package securefile creates and reads local secret files with a strict,
// platform-native access policy.
package securefile

import "errors"

// ErrInsecurePermissions reports a secret that may be reachable by another
// local account or through an unsafe filesystem alias.
var ErrInsecurePermissions = errors.New("secret file has insecure permissions")
