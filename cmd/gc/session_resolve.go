// session_resolve.go provides CLI-level session resolution.
// The core resolution logic lives in internal/session.ResolveSessionID.
package main

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// resolveSessionID delegates to session.ResolveSessionID.
func resolveSessionID(store beads.Store, identifier string) (string, error) {
	return session.ResolveSessionID(store, identifier)
}

func resolveSessionIDAllowClosed(store beads.Store, identifier string) (string, error) {
	return session.ResolveSessionIDAllowClosed(store, identifier)
}

type namedSessionResolveOptions struct {
	allowClosed         bool
	materialize         bool
	materializeMetadata map[string]string
}

const templateTargetPrefix = "template:"

type templateTarget struct {
	template   string
	forceFresh bool
}

var errNamedSessionConflict = errors.New("configured named session conflict")

func resolveConfiguredNamedSessionID(
	cityPath string,
	cfg *config.City,
	store beads.Store,
	identifier string,
	opts namedSessionResolveOptions,
) (string, bool, error) {
	if cfg == nil || store == nil {
		return "", false, fmt.Errorf("%w: %q", session.ErrSessionNotFound, identifier)
	}
	cityName := config.EffectiveCityName(cfg, filepath.Base(cityPath))
	spec, ok, err := findNamedSessionSpecForTarget(cfg, cityName, identifier)
	if err != nil {
		return "", false, err
	}
	if !ok {
		return "", false, fmt.Errorf("%w: %q", session.ErrSessionNotFound, identifier)
	}
	lookup, err := session.LookupConfiguredNamedSession(store, spec)
	if err != nil {
		return "", true, fmt.Errorf("looking up configured named session: %w", err)
	}
	if lookup.HasCanonical {
		return lookup.Canonical.ID, true, nil
	}
	// When materializing, check for a closed bead with this identity and
	// reopen it (preserves bead ID for reference continuity).
	if opts.materialize {
		if bead, _, ok := reopenClosedConfiguredNamedSessionBead(
			cityPath, store, cfg, cityName, spec.Identity, spec.SessionName, "stopped", time.Now().UTC(), opts.materializeMetadata, io.Discard,
		); ok {
			return bead.ID, true, nil
		}
	}
	if lookup.HasConflict {
		return "", true, namedSessionConflictError(identifier, spec, lookup.Conflict)
	}
	if !opts.materialize {
		return "", false, fmt.Errorf("%w: %q", session.ErrSessionNotFound, identifier)
	}
	id, err := ensureSessionIDForTemplateWithOptions(cityPath, cfg, store, spec.Identity, io.Discard, ensureSessionForTemplateOptions{
		materializeMetadata: opts.materializeMetadata,
	})
	return id, true, err
}

// namedSessionConflictError explains a named-session conflict well enough to
// act on. Every by-name verb refuses on a conflict, kill included, and kill
// could not clear one anyway: it stops a runtime, and an asleep bead is still
// live. The verb that frees the name is close, addressed by bead ID. But close
// stops the runtime, ends the bead for good and releases its work, and two of
// the three conflict kinds can be the seat's own live session. So the error
// prints what it takes to tell the kinds apart and recommends close only for
// a squat: a bead that records a template or agent other than this seat's
// (ga-lm5coj).
func namedSessionConflictError(identifier string, spec namedSessionSpec, b beads.Bead) error {
	d := session.DescribeNamedSessionConflict(b, spec)
	head := fmt.Sprintf("%q conflicts with configured named session %q via live bead %s (state=%q template=%q pool_managed=%q)",
		identifier, spec.Identity, b.ID, d.State, d.Template, d.PoolManaged)
	var advice string
	switch {
	case d.Kind == session.NamedSessionConflictAdoptablePool:
		advice = fmt.Sprintf("it is a pool-managed session of this seat's template that the reconciler adopts as %s; do not close it: retry after the next reconcile, or check 'gc session show %s'",
			spec.Identity, b.ID)
	case d.Squat:
		advice = fmt.Sprintf("it holds this seat's name for a different template (a name squat); if it is stale, close it with 'gc session close %s' to free the name for %s",
			b.ID, spec.Identity)
	default:
		advice = fmt.Sprintf("it records no template that rules it out as this seat's own running session; check 'gc session show %s' first, and do not close a session the seat is using",
			b.ID)
	}
	return fmt.Errorf("%w: %s; %s", errNamedSessionConflict, head, advice)
}

func resolveSessionIDWithConfig(cityPath string, cfg *config.City, store beads.Store, identifier string) (string, error) {
	return resolveSessionIDWithOptions(cityPath, cfg, store, identifier, namedSessionResolveOptions{})
}

func resolveSessionIDAllowClosedWithConfig(cityPath string, cfg *config.City, store beads.Store, identifier string) (string, error) {
	return resolveSessionIDWithOptions(cityPath, cfg, store, identifier, namedSessionResolveOptions{allowClosed: true})
}

func resolveSessionIDMaterializingNamed(cityPath string, cfg *config.City, store beads.Store, identifier string) (string, error) {
	return resolveSessionIDWithOptions(cityPath, cfg, store, identifier, namedSessionResolveOptions{materialize: true})
}

func resolveSessionIDMaterializingNamedWithMetadata(cityPath string, cfg *config.City, store beads.Store, identifier string, metadata map[string]string) (string, error) {
	return resolveSessionIDWithOptions(cityPath, cfg, store, identifier, namedSessionResolveOptions{
		materialize:         true,
		materializeMetadata: metadata,
	})
}

func parseTemplateTarget(identifier string) (templateTarget, bool) {
	identifier = strings.TrimSpace(identifier)
	if !strings.HasPrefix(identifier, templateTargetPrefix) {
		return templateTarget{}, false
	}
	name := normalizeNamedSessionTarget(strings.TrimSpace(strings.TrimPrefix(identifier, templateTargetPrefix)))
	if name == "" {
		return templateTarget{}, false
	}
	return templateTarget{
		template:   name,
		forceFresh: true,
	}, true
}

func resolveSessionIDWithOptions(
	cityPath string,
	cfg *config.City,
	store beads.Store,
	identifier string,
	opts namedSessionResolveOptions,
) (string, error) {
	if store == nil {
		return "", fmt.Errorf("session store unavailable")
	}
	if _, ok := parseTemplateTarget(identifier); ok {
		return "", fmt.Errorf("%w: %q", session.ErrSessionNotFound, identifier)
	}
	if id, err := session.ResolveSessionIDByExactID(store, identifier); err == nil {
		return id, nil
	} else if !errors.Is(err, session.ErrSessionNotFound) {
		return "", err
	}
	if id, matched, err := resolveConfiguredNamedSessionID(cityPath, cfg, store, identifier, opts); err == nil {
		return id, nil
	} else if matched || !errors.Is(err, session.ErrSessionNotFound) {
		return "", fmt.Errorf("resolving configured named session %q: %w", identifier, err)
	}
	if id, err := session.ResolveSessionID(store, identifier); err == nil {
		if cfg != nil {
			if info, getErr := sessionFrontDoor(store).Get(id); getErr == nil {
				if isNamedSessionInfo(info) {
					identity := namedSessionIdentityInfo(info)
					if identity != "" && config.FindNamedSession(cfg, identity) == nil {
						return "", fmt.Errorf("%w: %q", session.ErrSessionNotFound, identifier)
					}
				}
			}
		}
		return id, nil
	} else if !errors.Is(err, session.ErrSessionNotFound) {
		return "", err
	}
	if id, err := resolveOpenQualifiedAliasBasename(store, identifier); err == nil {
		return id, nil
	} else if !errors.Is(err, session.ErrSessionNotFound) {
		return "", err
	}
	if opts.allowClosed {
		if cfg != nil {
			cityName := config.EffectiveCityName(cfg, filepath.Base(cityPath))
			if _, ok, err := findNamedSessionSpecForTarget(cfg, cityName, identifier); err != nil {
				return "", err
			} else if ok {
				return "", fmt.Errorf("%w: %q", session.ErrSessionNotFound, identifier)
			}
		}
		if id, err := session.ResolveSessionIDAllowClosed(store, identifier); err == nil {
			return id, nil
		} else if !errors.Is(err, session.ErrSessionNotFound) {
			return "", err
		}
	}
	if identity := namedSessionIdentityForConfigName(cityPath, cfg, identifier); identity != "" {
		// Sessions are addressed by named-session identity, never by agent
		// config name (#666 keeps template names unresolved on this surface).
		// Say which name is wanted instead of a bare not-found (ga-lm5coj).
		return "", fmt.Errorf("%w: %q is an agent config name; its configured named session is %q", session.ErrSessionNotFound, identifier, identity)
	}
	return "", fmt.Errorf("%w: %q", session.ErrSessionNotFound, identifier)
}

// namedSessionIdentityForConfigName returns the identity of the one configured
// named session backed by the agent config name target (e.g.
// "qcore/cherub-law.archer" -> "qcore/archer"), or "" when none or several
// are, or when target is itself a configured named-session identity (then it
// is the right name, merely not materialized, and pointing elsewhere is wrong).
func namedSessionIdentityForConfigName(cityPath string, cfg *config.City, target string) string {
	if cfg == nil {
		return ""
	}
	if _, ok, err := findNamedSessionSpecForTarget(cfg, config.EffectiveCityName(cfg, filepath.Base(cityPath)), target); ok || err != nil {
		return ""
	}
	target = normalizeNamedSessionTarget(target)
	identity := ""
	for i := range cfg.NamedSessions {
		ns := &cfg.NamedSessions[i]
		if ns.TemplateQualifiedName() != target || ns.QualifiedName() == target {
			continue
		}
		if identity != "" && identity != ns.QualifiedName() {
			return ""
		}
		identity = ns.QualifiedName()
	}
	return identity
}

func resolveOpenQualifiedAliasBasename(store beads.Store, identifier string) (string, error) {
	identifier = strings.TrimSpace(identifier)
	if store == nil || identifier == "" || strings.Contains(identifier, "/") {
		return "", fmt.Errorf("%w: %q", session.ErrSessionNotFound, identifier)
	}
	sessFront := sessionFrontDoor(store)
	all, err := sessFront.ListAll(session.ListAllOptions{})
	if err != nil {
		return "", fmt.Errorf("listing sessions: %w", err)
	}
	matches := make([]session.Info, 0, 1)
	for _, info := range all {
		// ListAll already filters via IsSessionBeadOrRepairable and excludes
		// closed beads; the info.Closed guard is kept defensively.
		if info.Closed {
			continue
		}
		if info.Type == "" {
			sessFront.RepairTypeBestEffort(info.ID)
		}
		alias := strings.TrimSpace(info.Alias)
		if alias == "" || !strings.Contains(alias, "/") || session.TargetBasename(alias) != identifier {
			continue
		}
		matches = append(matches, info)
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("%w: %q", session.ErrSessionNotFound, identifier)
	case 1:
		return matches[0].ID, nil
	default:
		labels := make([]string, 0, len(matches))
		for _, match := range matches {
			labels = append(labels, fmt.Sprintf("%s (%s)", match.ID, strings.TrimSpace(match.Alias)))
		}
		return "", fmt.Errorf("%w: %q matches %d sessions: %s", session.ErrAmbiguous, identifier, len(matches), strings.Join(labels, ", "))
	}
}
