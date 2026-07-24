package server

// CONTRACT-10 — navigation model (single source of truth for the admin sidebar).
//
// This file is PRESENTATION ONLY. It adds no route, no handler, and no
// authorization: it merely describes, in ONE place, the sections and sub-options
// that already exist as HTTP routes since CONTRACT-07/08/09, so the shared layout
// can render a WordPress-style sidebar without every template hardcoding the menu.
//
// Active-section/active-item detection is INFERRED from the request path (carried
// on pageData.Path), centralized in the pageData.Nav() method below — a single
// place that maps a path to the highlighted section/sub-item. Handlers therefore
// only pass their own request path (Path: r.URL.Path); they never restate the
// menu structure. Rationale for path-inference over an explicit per-handler
// section key: one field (Path) yields BOTH the active section AND the active
// sub-item, and the section-level pages (e.g. an article edit form whose path is
// not itself a menu entry) still resolve to the right section by prefix without
// any extra bookkeeping in the handler.

import "strings"

// navItem is one sub-option (leaf) under a section.
type navItem struct {
	Label string
	Href  string
}

// navSection is one top-level entry in the sidebar. Children is empty for
// sections that have no submenu (Inicio, Roles y permisos).
type navSection struct {
	Label    string
	Href     string
	Children []navItem
}

// navSections is the SINGLE source of truth for the admin menu. It reflects the
// existing routes EXACTLY (see CONTRACT-10 RECON) — no new route is introduced
// here. Order is the display order in the sidebar.
var navSections = []navSection{
	{Label: "Inicio", Href: "/"},
	{Label: "Artículos", Href: "/admin/articles", Children: []navItem{
		{Label: "Todos los artículos", Href: "/admin/articles"},
		{Label: "Añadir nuevo", Href: "/admin/articles/new"},
	}},
	{Label: "Usuarios", Href: "/admin/users", Children: []navItem{
		{Label: "Todos los usuarios", Href: "/admin/users"},
		{Label: "Añadir nuevo", Href: "/admin/users/new"},
	}},
	{Label: "Roles y permisos", Href: "/admin/roles"},
	{Label: "API keys", Href: "/admin/api-keys", Children: []navItem{
		{Label: "Todas las keys", Href: "/admin/api-keys"},
		{Label: "Crear nueva", Href: "/admin/api-keys/new"},
	}},
}

// navItemView / navSectionView are the per-request view models the layout ranges
// over: the static menu plus the computed Active flags for the current path.
type navItemView struct {
	Label  string
	Href   string
	Active bool
}

type navSectionView struct {
	Label    string
	Href     string
	Active   bool
	Children []navItemView
}

// sectionActive reports whether the section at sectionHref owns the current path.
// Home ("/") matches ONLY the exact root so it is not "active everywhere"; every
// other section matches its own path or any sub-path beneath it (so an article
// edit form at /admin/articles/{id}/edit still lights up the Artículos section).
func sectionActive(sectionHref, path string) bool {
	if sectionHref == "/" {
		return path == "/"
	}
	return path == sectionHref || strings.HasPrefix(path, sectionHref+"/")
}

// Nav returns the sidebar view models for this page's request path. It is the
// ONE place that turns a path into highlighted section/sub-item state; the
// template stays declarative (range + .Active). A submenu item is active only on
// an exact path match, so "Todos los artículos" (/admin/articles) and "Añadir
// nuevo" (/admin/articles/new) never both highlight.
func (p pageData) Nav() []navSectionView {
	out := make([]navSectionView, 0, len(navSections))
	for _, s := range navSections {
		sv := navSectionView{
			Label:  s.Label,
			Href:   s.Href,
			Active: sectionActive(s.Href, p.Path),
		}
		for _, c := range s.Children {
			sv.Children = append(sv.Children, navItemView{
				Label:  c.Label,
				Href:   c.Href,
				Active: c.Href == p.Path,
			})
		}
		out = append(out, sv)
	}
	return out
}
