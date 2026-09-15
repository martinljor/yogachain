package chain

// retention.go — vista de retencion GFS: los puntos GFS del pivot agrupados por
// nivel (semanal / mensual / anual) con la fecha de vencimiento estimada, los que
// vencen pronto y los periodos en los que falta el GFS esperado.
//
// La REST API dice QUE punto es GFS (BackupFileModel.gfsPeriods) pero no HASTA
// CUANDO se retiene; el vencimiento se estima con la politica del job
// (storage.gfsPolicy.keepForNumberOf{Weeks,Months,Years}) desde la creacion del
// punto. Para un backup copy vale la politica del propio job de copia.

import (
	"context"
	"fmt"
	"sort"
	"time"

	"yogachain/internal/vbr"
)

type RetentionPoint struct {
	RestorePoint
	Expires  string `json:"expires,omitempty"` // RFC3339 estimado; vacio si el job no tiene politica
	DaysLeft int    `json:"daysLeft"`          // negativos = ya deberia haber expirado
	Period   string `json:"period"`            // etiqueta del periodo: 2026-W37, 2026-09, 2026
}

type RetentionTier struct {
	Tier     string           `json:"tier"`     // Weekly | Monthly | Quarterly | Yearly
	Keep     int              `json:"keep"`     // cuantos periodos retiene la politica (0 = sin politica conocida)
	Points   []RetentionPoint `json:"points"`   // mas reciente primero
	Expected []string         `json:"expected"` // periodos que la politica deberia cubrir hoy
	Missing  []string         `json:"missing"`  // periodos esperados sin punto GFS
}

type Retention struct {
	Pivot    string           `json:"pivot"`
	ID       string           `json:"id"`
	Policy   GFSPolicy        `json:"policy"`   // del job (pivot job) o vacia
	Tiers    []RetentionTier  `json:"tiers"`
	Soon     []RetentionPoint `json:"soon"`     // vencen en <= 30 dias
	Total    int              `json:"total"`    // puntos GFS encontrados
	NoPolicy bool             `json:"noPolicy"` // ningun job del pivot tiene GFS configurado
	Note     string           `json:"note,omitempty"`
}

// LoadRetention arma la vista para un pivot (job | vm | repo) con toda la historia.
func LoadRetention(ctx context.Context, s *vbr.Session, inv *Inventory, pivot, id string) (*Retention, error) {
	rps, err := RestorePoints(ctx, s, inv, pivot, id, time.Now().AddDate(-10, 0, 0))
	if err != nil {
		return nil, err
	}
	out := &Retention{Pivot: pivot, ID: id}
	if pivot == "job" {
		if j := inv.Job(id); j != nil {
			out.Policy = j.GFSPolicy
		}
	}
	now := time.Now()
	byTier := map[string][]RetentionPoint{}
	anyPolicy := false
	for _, rp := range rps {
		if rp.GFS == "" {
			continue
		}
		out.Total++
		pol := GFSPolicy{}
		if j := inv.Job(rp.JobID); j != nil {
			pol = j.GFSPolicy
		}
		if pol.Enabled {
			anyPolicy = true
		}
		t, err := time.Parse(time.RFC3339, rp.Date)
		if err != nil {
			continue
		}
		p := RetentionPoint{RestorePoint: rp, Period: periodLabel(rp.GFS, t)}
		if exp, ok := expiry(rp.GFS, t, pol); ok {
			p.Expires = exp.Format(time.RFC3339)
			p.DaysLeft = int(exp.Sub(now).Hours() / 24)
			if p.DaysLeft >= 0 && p.DaysLeft <= 30 {
				out.Soon = append(out.Soon, p)
			}
		}
		byTier[rp.GFS] = append(byTier[rp.GFS], p)
	}
	out.NoPolicy = !anyPolicy && out.Total == 0
	for _, tier := range []string{"Weekly", "Monthly", "Quarterly", "Yearly"} {
		pts := byTier[tier]
		keep := keepFor(tier, out.Policy)
		if len(pts) == 0 && keep == 0 {
			continue
		}
		sort.Slice(pts, func(a, b int) bool { return pts[a].Date > pts[b].Date })
		rt := RetentionTier{Tier: tier, Keep: keep, Points: pts}
		if keep > 0 {
			have := map[string]bool{}
			for _, p := range pts {
				have[p.Period] = true
			}
			for i := 0; i < keep; i++ {
				lbl := periodLabel(tier, shift(tier, now, -i))
				rt.Expected = append(rt.Expected, lbl)
				if !have[lbl] {
					rt.Missing = append(rt.Missing, lbl)
				}
			}
		}
		out.Tiers = append(out.Tiers, rt)
	}
	sort.Slice(out.Soon, func(a, b int) bool { return out.Soon[a].DaysLeft < out.Soon[b].DaysLeft })
	if pivot != "job" {
		out.Note = "Expiry is estimated from each job's GFS policy; expected/missing periods are computed only for the Job pivot."
	}
	return out, nil
}

func keepFor(tier string, p GFSPolicy) int {
	switch tier {
	case "Weekly":
		return p.Weekly
	case "Monthly":
		return p.Monthly
	case "Yearly":
		return p.Yearly
	}
	return 0
}

// expiry: creacion + retencion del nivel. Quarterly no tiene keep en la politica REST.
func expiry(tier string, t time.Time, p GFSPolicy) (time.Time, bool) {
	switch {
	case tier == "Weekly" && p.Weekly > 0:
		return t.AddDate(0, 0, 7*p.Weekly), true
	case tier == "Monthly" && p.Monthly > 0:
		return t.AddDate(0, p.Monthly, 0), true
	case tier == "Yearly" && p.Yearly > 0:
		return t.AddDate(p.Yearly, 0, 0), true
	}
	return time.Time{}, false
}

func periodLabel(tier string, t time.Time) string {
	switch tier {
	case "Weekly":
		y, w := t.ISOWeek()
		return fmt.Sprintf("%d-W%02d", y, w)
	case "Monthly":
		return t.Format("2006-01")
	case "Quarterly":
		return fmt.Sprintf("%d-Q%d", t.Year(), (int(t.Month())-1)/3+1)
	}
	return t.Format("2006")
}

func shift(tier string, t time.Time, n int) time.Time {
	switch tier {
	case "Weekly":
		return t.AddDate(0, 0, 7*n)
	case "Monthly":
		return t.AddDate(0, n, 0)
	case "Quarterly":
		return t.AddDate(0, 3*n, 0)
	}
	return t.AddDate(n, 0, 0)
}
