package requestpolicy

import (
	"github.com/inferplane/inferplane/internal/filter"
	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/internal/sensitivity"
)

// CombinedRedactor ensures policy masking never exempts a separately enabled
// legacy detector. Router still reinspects its result before allowing egress.
func CombinedRedactor(mask *filter.Masking, team string) router.RequestRedactor {
	if !mask.Enabled(team) {
		return nil
	}
	return sensitivity.NewRedactorWithMasker(mask.Filter)
}
