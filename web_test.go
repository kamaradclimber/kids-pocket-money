package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseAmount(t *testing.T) {
	valid := map[string]int64{
		"12,50":  1250,
		"12.50":  1250,
		"12,5":   1250,
		"12":     1200,
		"-3":     -300,
		"0,99":   99,
		"10 000": 1000000,
		"1e3":    100000,
	}
	for input, want := range valid {
		got, err := parseAmount(input)
		if err != nil {
			t.Errorf("parseAmount(%q): unexpected error %v", input, err)
			continue
		}
		if got != want {
			t.Errorf("parseAmount(%q) = %d, want %d", input, got, want)
		}
	}

	invalid := []string{"", "abc", "1,2,3", "12,50,30"}
	for _, input := range invalid {
		if _, err := parseAmount(input); err == nil {
			t.Errorf("parseAmount(%q): expected an error", input)
		}
	}

	// Very large numbers must not overflow int64.
	if _, err := parseAmount("99999999999999999999999"); err == nil {
		t.Error("expected an error for a huge amount")
	}
}

func TestEuro(t *testing.T) {
	cases := map[int64]string{
		1250:    "12,50 €",
		-550:    "-5,50 €",
		100:     "1,00 €",
		0:       "0,00 €",
		-105:    "-1,05 €",
		1000000: "10000,00 €",
	}
	for cents, want := range cases {
		if got := euro(cents); got != want {
			t.Errorf("euro(%d) = %q, want %q", cents, got, want)
		}
	}
}

func TestHasGroup(t *testing.T) {
	r, _ := http.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Remote-Groups", "kids, parents ,some-admin")
	if !hasGroup(r, "parents") {
		t.Error("expected parents to be detected")
	}
	if hasGroup(r, "parent") {
		t.Error("did not expect parent")
	}
}

func TestValidMonth(t *testing.T) {
	valid := []string{"2025-01", "2024-12", "1999-06"}
	for _, s := range valid {
		if !validMonth(s) {
			t.Errorf("expected %s to be valid", s)
		}
	}
	invalid := []string{"2025-13", "2025-00", "2025-1", "25-01", "2025/01", ""}
	for _, s := range invalid {
		if validMonth(s) {
			t.Errorf("expected %s to be invalid", s)
		}
	}
}

// testApp builds an app backed by a temporary store.
func testApp(t *testing.T) *app {
	t.Helper()
	st, err := newStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := config{ListenAddr: ":0", DataDir: t.TempDir(), AdminGroup: "parents", KidsGroup: "kids"}
	a, err := newApp(cfg, st)
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	return a
}

func request(t *testing.T, a *app, method, target, user, groups string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	var req *http.Request
	if body != nil {
		req = httptest.NewRequest(method, target, body)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req = httptest.NewRequest(method, target, nil)
	}
	req.Header.Set("Remote-User", user)
	if groups != "" {
		req.Header.Set("Remote-Groups", groups)
	}
	// Establish the CSRF cookie on the session.
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: strings.Repeat("a", 64)})
	rec := httptest.NewRecorder()
	a.routes().ServeHTTP(rec, req)
	return rec
}

func TestIndexRequiresRemoteUser(t *testing.T) {
	a := testApp(t)
	rec := request(t, a, http.MethodGet, "/", "", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 without Remote-User, got %d", rec.Code)
	}
}

func TestAccessDeniedWithoutKidsOrParentsGroup(t *testing.T) {
	a := testApp(t)
	// The whole app is closed to users with neither the kids nor the
	// parents group — even known usernames and POST endpoints.
	if _, err := a.store.createKid("alice", "Alice", 1000, time.Now().Format("2006-01"), "parent", time.Now()); err != nil {
		t.Fatalf("createKid: %v", err)
	}
	for _, target := range []string{"/", "/enfant/alice", "/nope"} {
		rec := request(t, a, http.MethodGet, target, "alice", "some-other-group", nil)
		if rec.Code != http.StatusForbidden {
			t.Errorf("GET %s: expected 403 without kids/parents group, got %d", target, rec.Code)
		}
	}
	form := url.Values{
		"csrf_token": {strings.Repeat("a", 64)},
		"kind":       {"spend"},
		"amount":     {"1"},
		"label":      {"x"},
	}
	rec := request(t, a, http.MethodPost, "/enfant/alice/transaction", "alice", "some-other-group", form)
	if rec.Code != http.StatusForbidden {
		t.Errorf("POST /enfant/alice/transaction: expected 403 without kids/parents group, got %d", rec.Code)
	}
}

func TestHealthProbesStayPublic(t *testing.T) {
	a := testApp(t)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	a.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 on /healthz without auth, got %d", rec.Code)
	}
}

func TestUnknownKidGets403(t *testing.T) {
	a := testApp(t)
	rec := request(t, a, http.MethodGet, "/", "stranger", "kids", nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 for unknown kid, got %d", rec.Code)
	}
}

func TestKidWithoutGroupIsForbidden(t *testing.T) {
	a := testApp(t)
	// Even a registered kid username is refused without the kids group.
	if _, err := a.store.createKid("alice", "Alice", 1000, time.Now().Format("2006-01"), "parent", time.Now()); err != nil {
		t.Fatalf("createKid: %v", err)
	}
	rec := request(t, a, http.MethodGet, "/", "alice", "", nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 without the kids group, got %d", rec.Code)
	}
}

func TestKidSeesOnlyOwnMoney(t *testing.T) {
	a := testApp(t)
	month := time.Now().Format("2006-01")
	if _, err := a.store.createKid("alice", "Alice", 1000, month, "parent", time.Now()); err != nil {
		t.Fatalf("createKid: %v", err)
	}
	if _, err := a.store.createKid("bob", "Bob", 50000, month, "parent", time.Now()); err != nil {
		t.Fatalf("createKid: %v", err)
	}

	body := request(t, a, http.MethodGet, "/", "alice", "kids", nil).Body.String()
	if !strings.Contains(body, "Alice") {
		t.Error("kid dashboard should render the kid's own name")
	}
	if strings.Contains(body, "Bob") {
		t.Error("kid dashboard must not show other kids")
	}
	if !strings.Contains(body, "10,00 €") {
		t.Error("kid dashboard should render the own balance")
	}
	if strings.Contains(body, "500,00 €") {
		t.Error("kid dashboard must not render another kid's balance")
	}
}

func TestParentSeesAllKidsAtOnce(t *testing.T) {
	a := testApp(t)
	month := time.Now().Format("2006-01")
	if _, err := a.store.createKid("alice", "Alice", 1000, month, "parent", time.Now()); err != nil {
		t.Fatalf("createKid: %v", err)
	}
	if _, err := a.store.createKid("bob", "Bob", 500, month, "parent", time.Now()); err != nil {
		t.Fatalf("createKid: %v", err)
	}

	body := request(t, a, http.MethodGet, "/", "papa", "parents", nil).Body.String()
	for _, want := range []string{"Alice", "10,00 €", "Bob", "5,00 €"} {
		if !strings.Contains(body, want) {
			t.Errorf("parent dashboard should show %q (all kids at once)", want)
		}
	}
}

func TestKidViewsOwnDetailButNotOthers(t *testing.T) {
	a := testApp(t)
	month := time.Now().Format("2006-01")
	if _, err := a.store.createKid("alice", "Alice", 1000, month, "parent", time.Now()); err != nil {
		t.Fatalf("createKid: %v", err)
	}
	if _, err := a.store.createKid("bob", "Bob", 500, month, "parent", time.Now()); err != nil {
		t.Fatalf("createKid: %v", err)
	}
	_, _ = a.store.ensureMonthlyAllowances(time.Now())

	// A kid sees their own page, without the parent-only forms.
	body := request(t, a, http.MethodGet, "/enfant/alice", "alice", "kids", nil).Body.String()
	for _, want := range []string{"Alice", "10,00 €", "Historique"} {
		if !strings.Contains(body, want) {
			t.Errorf("kid own detail page should show %q", want)
		}
	}
	for _, banned := range []string{"Nouveau montant mensuel", "Enregistrer l'opération", "Enregistrer le montant", "Supprimer", "/transaction/"} {
		if strings.Contains(body, banned) {
			t.Errorf("kid own detail page must not contain %q (parent-only)", banned)
		}
	}

	// Another kid's page is 403 (it is fine to confirm a sibling exists);
	// unknown usernames stay 404.
	for _, tc := range []struct{ user, groups, target string }{
		{"alice", "kids", "/enfant/bob"},
	} {
		rec := request(t, a, http.MethodGet, tc.target, tc.user, tc.groups, nil)
		if rec.Code != http.StatusForbidden {
			t.Errorf("GET %s as %s: expected 403, got %d", tc.target, tc.user, rec.Code)
		}
	}
	if rec := request(t, a, http.MethodGet, "/enfant/inconnu", "alice", "kids", nil); rec.Code != http.StatusNotFound {
		t.Errorf("GET /enfant/inconnu as alice: expected 404, got %d", rec.Code)
	}
	rec := request(t, a, http.MethodGet, "/enfant/bob", "papa", "parents", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for a parent on /enfant/bob, got %d", rec.Code)
	}
	if rec2 := request(t, a, http.MethodGet, "/enfant/inconnu", "papa", "parents", nil); rec2.Code != http.StatusNotFound {
		t.Errorf("expected 404 for an unknown kid, got %d", rec2.Code)
	}
}

func TestKidDashboard(t *testing.T) {
	a := testApp(t)
	// Use the real current month so the dashboard's own ensure pass (which
	// uses time.Now()) agrees with the test data.
	k, err := a.store.createKid("alice", "Alice", 1000, time.Now().Format("2006-01"), "parent", time.Now())
	if err != nil {
		t.Fatalf("createKid: %v", err)
	}

	rec := request(t, a, http.MethodGet, "/", "alice", "kids", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Alice") {
		t.Error("kid dashboard should render the kid's name")
	}
	balance, err := a.store.balance(k.ID)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if balance != 1000 { // exactly one month credited
		t.Errorf("expected balance 1000 cents, got %d", balance)
	}
	if !strings.Contains(body, euro(balance)) {
		t.Errorf("kid dashboard should render balance %s, body:\n%s", euro(balance), body)
	}
}

func TestParentDashboard(t *testing.T) {
	a := testApp(t)
	month := time.Now().Format("2006-01")
	if _, err := a.store.createKid("alice", "Alice", 1000, month, "parent", time.Now()); err != nil {
		t.Fatalf("createKid: %v", err)
	}
	rec := request(t, a, http.MethodGet, "/", "parent@example", "parents", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Ajouter un enfant") {
		t.Error("parent dashboard should render the create-kid form")
	}
	// Read-only recap: name, balance and the link to the kid page.
	for _, want := range []string{"Alice", "10,00 €", "Voir le détail et l'historique", "/enfant/alice"} {
		if !strings.Contains(body, want) {
			t.Errorf("parent dashboard should show %q", want)
		}
	}
	// No transaction form on the family view: adding an operation is done
	// from the kid detail page.
	if strings.Contains(body, "Enregistrer l'opération") {
		t.Error("parent dashboard must not render the transaction form")
	}
}

func TestCreateKidAndAllowanceFlow(t *testing.T) {
	a := testApp(t)
	form := url.Values{
		"csrf_token":     {strings.Repeat("a", 64)},
		"username":       {"bob"},
		"name":           {"Bob"},
		"monthly_amount": {"10"},
	}
	rec := request(t, a, http.MethodPost, "/kid", "parent", "parents", form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect, got %d", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/enfant/") {
		t.Fatalf("expected redirect to kid page, got %q", loc)
	}

	// Allowance for the current month was credited at creation time.
	txs, err := a.store.listTransactions(1)
	if err != nil {
		t.Fatalf("listTransactions: %v", err)
	}
	if len(txs) != 1 || txs[0].Kind != txKindAllowance || txs[0].Amount != 1000 {
		t.Errorf("expected one allowance of 1000 cents, got %+v", txs)
	}
}

func TestCreateKidRequiresAdmin(t *testing.T) {
	a := testApp(t)
	form := url.Values{
		"csrf_token":     {strings.Repeat("a", 64)},
		"username":       {"bob"},
		"name":           {"Bob"},
		"monthly_amount": {"10"},
	}
	rec := request(t, a, http.MethodPost, "/kid", "alice", "", form)
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 for non-admin, got %d", rec.Code)
	}
}

func TestKidCanAddOwnTransactionButNotOthers(t *testing.T) {
	a := testApp(t)
	now := time.Date(2025, time.November, 15, 10, 0, 0, 0, time.UTC)
	k, _ := a.store.createKid("alice", "Alice", 1000, "2025-11", "parent", now)
	k2, _ := a.store.createKid("bob", "Bob", 1000, "2025-11", "parent", now)

	_, _ = a.store.ensureMonthlyAllowances(now)

	form := url.Values{
		"csrf_token": {strings.Repeat("a", 64)},
		"kind":       {"spend"},
		"amount":     {"4,50"},
		"label":      {"bonbons"},
	}
	rec := request(t, a, http.MethodPost, "/enfant/alice/transaction", "alice", "kids", form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d: %s", rec.Code, rec.Body.String())
	}
	balance, _ := a.store.balance(k.ID)
	if balance != 550 { // 1000 allowance - 450 spend
		t.Errorf("expected 550, got %d", balance)
	}

	// Alice cannot act on Bob's account (403: it is fine to confirm a
	// sibling exists).
	rec = request(t, a, http.MethodPost, "/enfant/bob/transaction", "alice", "kids", form)
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 for another kid, got %d", rec.Code)
	}
	balance2, _ := a.store.balance(k2.ID)
	if balance2 != 1000 {
		t.Errorf("Bob's balance must be untouched, got %d", balance2)
	}
}

func TestKidCannotAddMoney(t *testing.T) {
	a := testApp(t)
	now := time.Now()
	k, err := a.store.createKid("alice", "Alice", 1000, now.Format("2006-01"), "parent", now)
	if err != nil {
		t.Fatalf("createKid: %v", err)
	}
	_, _ = a.store.ensureMonthlyAllowances(now)

	// A kid posting kind=add must be refused server-side, even if the
	// template never offers the choice.
	form := url.Values{
		"csrf_token": {strings.Repeat("a", 64)},
		"kind":       {"add"},
		"amount":     {"100"},
		"label":      {"j'aimerais être riche"},
	}
	rec := request(t, a, http.MethodPost, "/enfant/alice/transaction", "alice", "kids", form)
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 for a kid adding money, got %d", rec.Code)
	}
	balance, _ := a.store.balance(k.ID)
	if balance != 1000 {
		t.Errorf("balance must be untouched, got %d", balance)
	}

	// A parent, on the other hand, may add money.
	rec = request(t, a, http.MethodPost, "/enfant/alice/transaction", "papa", "parents", form)
	if rec.Code != http.StatusSeeOther {
		t.Errorf("expected 303 for a parent adding money, got %d: %s", rec.Code, rec.Body.String())
	}
	balance, _ = a.store.balance(k.ID)
	if balance != 11000 { // 1000 allowance + 100€ added by the parent
		t.Errorf("expected 11000 after parent addition, got %d", balance)
	}
}

func TestCSRFIsRequired(t *testing.T) {
	a := testApp(t)
	form := url.Values{
		"csrf_token": {"wrong-token-value-aaaaaaaaaaaaaaaaaaaaaaa"},
		"kind":       {"spend"},
		"amount":     {"1"},
		"label":      {"test"},
	}
	_, err := a.store.createKid("alice", "Alice", 1000, "2025-11", "parent", time.Now())
	if err != nil {
		t.Fatalf("createKid: %v", err)
	}
	rec := request(t, a, http.MethodPost, "/enfant/alice/transaction", "alice", "", form)
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 with a bad CSRF token, got %d", rec.Code)
	}
}

func TestAllowanceNotDeletableFromUI(t *testing.T) {
	a := testApp(t)
	now := time.Date(2025, time.November, 15, 10, 0, 0, 0, time.UTC)
	k, _ := a.store.createKid("alice", "Alice", 1000, "2025-11", "parent", now)
	_, _ = a.store.ensureMonthlyAllowances(now)
	txs, _ := a.store.listTransactions(k.ID)
	allowanceID := txs[0].ID

	form := url.Values{"csrf_token": {strings.Repeat("a", 64)}}
	// Allowance rows are not reachable through the delete endpoint (only
	// manual transactions are), so the endpoint answers 404.
	rec := request(t, a, http.MethodPost, "/transaction/"+strconv.FormatInt(allowanceID, 10)+"/delete", "parent", "parents", form)
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 deleting an allowance (only manual txs are deletable), got %d", rec.Code)
	}
	balance, _ := a.store.balance(k.ID)
	if balance != 1000 {
		t.Errorf("allowance balance must be untouched, got %d", balance)
	}
}

func TestTransactionWithOptionalDate(t *testing.T) {
	a := testApp(t)
	now := time.Now()
	k, err := a.store.createKid("alice", "Alice", 1000, now.Format("2006-01"), "parent", now)
	if err != nil {
		t.Fatalf("createKid: %v", err)
	}

	// Backdated spend: date field set to the 3rd of the previous month.
	opDate := now.AddDate(0, -1, 0)
	for opDate.Day() != 3 {
		opDate = opDate.AddDate(0, 0, 1)
	}
	form := url.Values{
		"csrf_token": {strings.Repeat("a", 64)},
		"kind":       {"spend"},
		"amount":     {"2,50"},
		"label":      {"bonbons"},
		"date":       {opDate.Format("2006-01-02")},
	}
	rec := request(t, a, http.MethodPost, "/enfant/alice/transaction", "alice", "kids", form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d: %s", rec.Code, rec.Body.String())
	}
	txs, _ := a.store.listTransactions(k.ID)
	found := false
	for _, tx := range txs {
		if tx.Label == "bonbons" {
			found = true
			if tx.Date != opDate.Format("2006-01-02") {
				t.Errorf("expected date %s, got %q", opDate.Format("2006-01-02"), tx.Date)
			}
			if tx.Month != opDate.Format("2006-01") {
				t.Errorf("expected month %s, got %q", opDate.Format("2006-01"), tx.Month)
			}
		}
	}
	if !found {
		t.Fatal("transaction was not stored")
	}
}

func TestTransactionInvalidDateRejected(t *testing.T) {
	a := testApp(t)
	now := time.Now()
	k, err := a.store.createKid("alice", "Alice", 1000, now.Format("2006-01"), "parent", now)
	if err != nil {
		t.Fatalf("createKid: %v", err)
	}
	_, _ = a.store.ensureMonthlyAllowances(now)

	form := url.Values{
		"csrf_token": {strings.Repeat("a", 64)},
		"kind":       {"spend"},
		"amount":     {"2,50"},
		"label":      {"bonbons"},
		"date":       {"31/02/2025"}, // French day-first format must be rejected
	}
	rec := request(t, a, http.MethodPost, "/enfant/alice/transaction", "alice", "kids", form)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid date, got %d", rec.Code)
	}
	// Nothing was stored.
	balance, _ := a.store.balance(k.ID)
	if balance != 1000 {
		t.Errorf("balance must be untouched after invalid date, got %d", balance)
	}

	// Out-of-range year rejected too.
	form.Set("date", "1999-12-31")
	rec = request(t, a, http.MethodPost, "/enfant/alice/transaction", "alice", "kids", form)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for out-of-range date, got %d", rec.Code)
	}
}

func TestNiceDate(t *testing.T) {
	if got := niceDate("2025-11-03"); got != "03/11/2025" {
		t.Errorf("niceDate(2025-11-03) = %q, want 03/11/2025", got)
	}
	if got := niceDate("not-a-date"); got != "not-a-date" {
		t.Errorf("niceDate fallback failed: %q", got)
	}
}

func TestTransactionShowsDisplayName(t *testing.T) {
	a := testApp(t)
	now := time.Now()
	if _, err := a.store.createKid("alice", "Alice", 1000, now.Format("2006-01"), "parent", now); err != nil {
		t.Fatalf("createKid: %v", err)
	}
	_, _ = a.store.ensureMonthlyAllowances(now)

	// A parent records a spend; authelia passes Remote-Name along.
	form := url.Values{
		"csrf_token": {strings.Repeat("a", 64)},
		"kind":       {"spend"},
		"amount":     {"4,50"},
		"label":      {"gloubix"},
	}
	req := httptest.NewRequest(http.MethodPost, "/enfant/alice/transaction",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Remote-User", "grego")
	req.Header.Set("Remote-Groups", "parents")
	req.Header.Set("Remote-Name", "Grégoire")
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: strings.Repeat("a", 64)})
	rec := httptest.NewRecorder()
	a.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d: %s", rec.Code, rec.Body.String())
	}

	// The kid's history shows the parent's display name, not the username.
	body := request(t, a, http.MethodGet, "/", "alice", "kids", nil).Body.String()
	if !strings.Contains(body, "Ajouté par Grégoire") {
		t.Errorf("kid history should show the display name Grégoire, got:\n%s", body)
	}
	if strings.Contains(body, "Ajouté par grego") {
		t.Error("kid history must not show the raw username")
	}
}

func TestRemoteNameFallsBackToUsername(t *testing.T) {
	r, _ := http.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Remote-User", "grego")
	if got := remoteName(r); got != "grego" {
		t.Errorf("expected fallback to username, got %q", got)
	}
	// Only the first name is kept ("Grégoire Seux" -> "Grégoire").
	r.Header.Set("Remote-Name", "  Grégoire Seux  ")
	if got := remoteName(r); got != "Grégoire" {
		t.Errorf("expected first name Grégoire, got %q", got)
	}
}

func TestExportCSV(t *testing.T) {
	a := testApp(t)
	now := time.Now()
	k, err := a.store.createKid("alice", "Alice", 1000, now.Format("2006-01"), "parent", now)
	if err != nil {
		t.Fatalf("createKid: %v", err)
	}
	opDate := time.Date(now.Year(), now.Month(), 3, 0, 0, 0, 0, time.Local)
	if _, err := a.store.addTransaction(k.ID, -450, "bonbons; \"speciaux\"", "Alice", "alice", opDate, now); err != nil {
		t.Fatalf("addTransaction: %v", err)
	}
	_, _ = a.store.ensureMonthlyAllowances(now)

	rec := request(t, a, http.MethodGet, "/enfant/alice/export.csv", "alice", "kids", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for own export, got %d", rec.Code)
	}
	body := rec.Body.String()
	if ct := rec.Header().Get("Content-Type"); ct != "text/csv; charset=utf-8" {
		t.Errorf("unexpected content type %q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment; filename=operations-alice.csv") {
		t.Errorf("unexpected content disposition %q", cd)
	}
	// BOM for French Excel, French header, semicolon separated, decimal
	// comma amounts, quoted label with doubled inner quotes.
	for _, want := range []string{
		"\uFEFFdate;libellé;type;montant",
		`"bonbons; ""speciaux"""`,
		"Opération;-4,50",
		"Argent de poche;10,00",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("export should contain %q, got:\n%s", want, body)
		}
	}
	// Chronological: oldest first (allowance date < bonbons date, both this
	// month, so compare positions).
	if strings.Index(body, "Argent de poche;10,00") > strings.Index(body, "Opération;-4,50") {
		t.Error("export should be chronological (allowance first)")
	}

	// Parent exports any kid.
	rec = request(t, a, http.MethodGet, "/enfant/alice/export.csv", "papa", "parents", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for a parent export, got %d", rec.Code)
	}
	// Another kid's export is 403 (bob exists); unknown kid is 404.
	if _, err := a.store.createKid("bob", "Bob", 500, now.Format("2006-01"), "parent", now); err != nil {
		t.Fatalf("createKid bob: %v", err)
	}
	rec = request(t, a, http.MethodGet, "/enfant/bob/export.csv", "alice", "kids", nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 exporting another kid, got %d", rec.Code)
	}
	rec = request(t, a, http.MethodGet, "/enfant/inconnu/export.csv", "papa", "parents", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 for unknown kid, got %d", rec.Code)
	}
}
