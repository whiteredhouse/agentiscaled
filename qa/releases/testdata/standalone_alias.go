// Added only to the immutable source copy used to build the CI companion.
// Aliases expose the existing implementation without changing its behavior.
package standalone

import internal "github.com/google/agentmesh/internal/standalone"

type Options = internal.Options
type Server = internal.Server

var New = internal.New
