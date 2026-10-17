package apply

import "time"

// changerTimeout bounds one READ ELEMENT STATUS, as the daemon's changer
// queries (internal/changer queryTimeout).
const changerTimeout = 30 * time.Second
