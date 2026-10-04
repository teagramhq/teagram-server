package srp

import "time"

// DefaultTTL is how long an issued SRP challenge stays valid. A client that
// takes longer between account.getPassword and auth.checkPassword must re-fetch.
const DefaultTTL = 5 * time.Minute
