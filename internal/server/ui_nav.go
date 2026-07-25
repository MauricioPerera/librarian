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

// CONTRACT-15 T1 — the menu stops being purely static. The sidebar must also
// list the DYNAMIC content types, and those live in the database, so Nav() is
// now "static sections + the per-request dynamic sections carried on
// pageData.dynamic". The dynamic half is filled EXCLUSIVELY by h.page(r, …)
// (ui.go), the single constructor every admin page goes through; see the long
// comment on pageData for why a page cannot silently lose those entries.

import (
	"context"
	"strings"

	"github.com/MauricioPerera/librarian/internal/schema"
	"github.com/MauricioPerera/librarian/internal/store"
)

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
	{Label: "Productos", Href: "/admin/products", Children: []navItem{
		{Label: "Todos los productos", Href: "/admin/products"},
		{Label: "Añadir nuevo", Href: "/admin/products/new"},
	}},
	{Label: "Categorías y tags", Href: "/admin/terms", Children: []navItem{
		{Label: "Todas las categorías y tags", Href: "/admin/terms"},
		{Label: "Añadir nueva", Href: "/admin/terms/new"},
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
	// CONTRACT-15 T2: managing the DEFINITIONS is a fixed pair of routes, so it
	// belongs in the static half. Only the per-type entries below it are dynamic.
	{Label: "Tipos de contenido", Href: "/admin/content-types", Children: []navItem{
		{Label: "Todos los tipos", Href: "/admin/content-types"},
		{Label: "Añadir nuevo", Href: "/admin/content-types/new"},
	}},
}

// dynamicNavSections turns the PERSISTED dynamic content-type definitions into
// sidebar sections, one per type, each with the same "list / add new" submenu
// the code-defined types have. The labels and hrefs are built from the type
// NAME, which is `[a-z][a-z0-9_]*` by the CONTRACT-13 gate and was re-validated
// on load, so nothing hostile can reach an href.
//
// A read failure yields NO dynamic entries rather than an error: this is the
// sidebar, and a database hiccup must not turn every admin page into a 500. The
// static sections (including "Tipos de contenido", from which the admin can see
// the real list) always render.
func (h *handlers) dynamicNavSections(ctx context.Context) []navSection {
	defs, err := store.LoadDefinitions(ctx, h.store)
	if err != nil {
		return nil
	}
	return navSectionsForTypes(defs)
}

// navSectionsForTypes is the pure half of dynamicNavSections (no database), so
// the mapping definition→menu entry is testable on its own.
func navSectionsForTypes(defs []schema.ContentTypeDefinition) []navSection {
	out := make([]navSection, 0, len(defs))
	for _, d := range defs {
		base := "/admin/content/" + d.Name
		out = append(out, navSection{
			Label: d.Name,
			Href:  base,
			Children: []navItem{
				{Label: "Todo el contenido", Href: base},
				{Label: "Añadir nuevo", Href: base + "/new"},
			},
		})
	}
	return out
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
// CONTRACT-15 T1: it ranges over the static sections FOLLOWED BY p.dynamic (the
// per-request entries filled by h.page). Active-state inference is identical for
// both halves — a dynamic type's section lights up on /admin/content/{type} and
// any path beneath it, exactly like Artículos or Productos.
func (p pageData) Nav() []navSectionView {
	sections := make([]navSection, 0, len(navSections)+len(p.dynamic))
	sections = append(sections, navSections...)
	sections = append(sections, p.dynamic...)
	out := make([]navSectionView, 0, len(sections))
	for _, s := range sections {
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
