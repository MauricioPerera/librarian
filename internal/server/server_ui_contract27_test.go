package server_test

// CONTRACT-27 T4 acceptance tests — declaring a relation from the admin panel,
// and the T3 guard as it is SEEN there.
//
// The two things that must be proven rather than described: an admin can create
// a type with a relation without touching the JSON API, and the panel refuses
// (and explains) the edit and the deletion of a referenced type instead of
// offering a button that cannot work.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// uiCreateTypeWithReference creates a dynamic type through the REAL create-form
// POST, including its relation rows.
func uiCreateTypeWithReference(t *testing.T, client *http.Client, srv *httptest.Server, name string, fields [][2]string, references [][2]string) (int, string) {
	t.Helper()
	form := url.Values{"name": {name}}
	for _, f := range fields {
		form.Add("field_name", f[0])
		form.Add("field_type", f[1])
	}
	for _, r := range references {
		form.Add("reference_name", r[0])
		form.Add("reference_target", r[1])
	}
	resp, err := client.PostForm(srv.URL+"/admin/content-types", form)
	if err != nil {
		t.Fatalf("create content type %q: %v", name, err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, string(body)
}

// TestAdminCanDeclareARelationFromThePanel is T4: the create form offers the
// existing types as targets and the submit produces a real relation.
func TestAdminCanDeclareARelationFromThePanel(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "administrator", "content_types.manage", "content.create")
	client := loginUI(t, db, srv, "admin@example.com", "pw", "administrator")

	// With no types yet, the form SAYS there is nothing to point at instead of
	// rendering an unusable selector.
	_, page := getBody(t, client, srv.URL+"/admin/content-types/new")
	if !strings.Contains(page, "no-reference-targets") {
		t.Fatalf("the empty-installation form does not explain that there is no target yet: %.600q", page)
	}

	uiCreateContentType(t, client, srv, "autores", [2]string{"nombre", "text"})

	// Now the form offers `autores` as a target.
	_, page = getBody(t, client, srv.URL+"/admin/content-types/new")
	if !strings.Contains(page, `name="reference_target"`) || !strings.Contains(page, `<option value="autores"`) {
		t.Fatalf("the create form does not offer the existing type as a relation target: %.900q", page)
	}
	// And the htmx fragment route returns one more row with the same options.
	_, fragment := getBody(t, client, srv.URL+"/admin/content-types/new/reference")
	if !strings.Contains(fragment, `name="reference_name"`) || !strings.Contains(fragment, `<option value="autores"`) {
		t.Fatalf("the relation-row fragment is not usable: %q", fragment)
	}

	status, body := uiCreateTypeWithReference(t, client, srv, "libros",
		[][2]string{{"titulo", "text"}}, [][2]string{{"autor", "autores"}})
	if status != http.StatusOK {
		t.Fatalf("create libros: status=%d %.500q", status, body)
	}
	// The list shows the relation.
	if !strings.Contains(body, "cell-references") || !strings.Contains(body, "→ autores") {
		t.Fatalf("the list does not show the relation: %.900q", body)
	}
	t.Log("OK: a relation was declared entirely from the panel and is listed")

	// A relation to a type that does not exist comes back as a FORM error, never
	// a 500 and never raw JSON.
	status, body = uiCreateTypeWithReference(t, client, srv, "resenas",
		[][2]string{{"texto", "text"}}, [][2]string{{"libro", "inexistente"}})
	if status != http.StatusBadRequest {
		t.Fatalf("a relation to a missing target: status=%d, want 400: %.400q", status, body)
	}
	if !strings.Contains(body, "No se pudo crear el tipo de contenido") {
		t.Fatalf("the form error is not rendered: %.600q", body)
	}
	t.Log("OK: an impossible relation is a form error, not a 500")
}

// TestAdminSeesTheGuardOnBothPages is T4's other half: the edit page and the
// delete page of a REFERENCED type explain why they are blocked and name the
// referrer, and neither offers its submit button.
func TestAdminSeesTheGuardOnBothPages(t *testing.T) {
	db, srv, cleanup := openUITLS(t)
	defer cleanup()
	grant(t, db, "administrator", "content_types.manage", "content.create")
	client := loginUI(t, db, srv, "admin@example.com", "pw", "administrator")

	uiCreateContentType(t, client, srv, "autores", [2]string{"nombre", "text"})
	if status, body := uiCreateTypeWithReference(t, client, srv, "libros",
		[][2]string{{"titulo", "text"}}, [][2]string{{"autor", "autores"}}); status != http.StatusOK {
		t.Fatalf("create libros: %d %.400q", status, body)
	}

	// The action control each page would normally offer. The layout has its own
	// (logout), so the assertion targets the page's own control by name.
	blocked := map[string]string{
		"/admin/content-types/autores/edit":   "Guardar cambios",
		"/admin/content-types/autores/delete": `id="confirm-delete"`,
	}
	for page, control := range blocked {
		_, body := getBody(t, client, srv.URL+page)
		if !strings.Contains(body, `id="referenced-block"`) {
			t.Fatalf("%s does not show the guard: %.900q", page, body)
		}
		if !strings.Contains(body, `class="referrer-type">libros<`) {
			t.Fatalf("%s does not name the referrer: %.900q", page, body)
		}
		if strings.Contains(body, control) {
			t.Fatalf("%s still offers %q for an operation that cannot work", page, control)
		}
		t.Logf("OK %s: blocked, names libros, %q is gone", page, control)
	}

	// A hand-crafted POST straight to the delete route is refused too — the panel
	// is not the only thing standing in the way.
	status, body := postDeleteForm(t, client, srv, "autores", url.Values{
		"confirm_name": {"autores"}, "confirm_rows": {"0"},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("a direct delete POST: status=%d, want 400: %.400q", status, body)
	}
	if !strings.Contains(body, `id="referenced-block"`) {
		t.Fatalf("the direct POST refusal does not explain itself: %.900q", body)
	}
	if !sqliteTableExists(t, db, "cpt_autores") {
		t.Fatal("the refused POST destroyed the table")
	}
	t.Log("OK: a direct POST is refused with the same explanation and nothing was destroyed")
}
