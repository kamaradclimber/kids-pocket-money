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
	for _, target := range []string{"/", "/kid/1", "/nope"} {
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
	rec := request(t, a, http.MethodPost, "/kid/1/transaction", "alice", "some-other-group", form)
	if rec.Code != http.StatusForbidden {
		t.Errorf("POST /kid/1/transaction: expected 403 without kids/parents group, got %d", rec.Code)
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

func TestKidCannotViewKidDetailPages(t *testing.T) {
	a := testApp(t)
	month := time.Now().Format("2006-01")
	if _, err := a.store.createKid("alice", "Alice", 1000, month, "parent", time.Now()); err != nil {
		t.Fatalf("createKid: %v", err)
	}
	bob, err := a.store.createKid("bob", "Bob", 500, month, "parent", time.Now())
	if err != nil {
		t.Fatalf("createKid: %v", err)
	}
	// Kid detail pages are parent-only: kids use their own dashboard.
	rec := request(t, a, http.MethodGet, "/kid/"+strconv.FormatInt(bob.ID, 10), "alice", "kids", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 for a kid viewing a kid detail page, got %d", rec.Code)
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
	rec := request(t, a, http.MethodGet, "/", "parent@example", "parents", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Ajouter un enfant") {
		t.Error("parent dashboard should render the create-kid form")
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
	if !strings.HasPrefix(loc, "/kid/") {
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
	rec := request(t, a, http.MethodPost, "/kid/"+strconv.FormatInt(k.ID, 10)+"/transaction", "alice", "kids", form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d: %s", rec.Code, rec.Body.String())
	}
	balance, _ := a.store.balance(k.ID)
	if balance != 550 { // 1000 allowance - 450 spend
		t.Errorf("expected 550, got %d", balance)
	}

	// Alice cannot act on Bob's account.
	rec = request(t, a, http.MethodPost, "/kid/"+strconv.FormatInt(k2.ID, 10)+"/transaction", "alice", "kids", form)
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 for another kid, got %d", rec.Code)
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
	rec := request(t, a, http.MethodPost, "/kid/"+strconv.FormatInt(k.ID, 10)+"/transaction", "alice", "kids", form)
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 for a kid adding money, got %d", rec.Code)
	}
	balance, _ := a.store.balance(k.ID)
	if balance != 1000 {
		t.Errorf("balance must be untouched, got %d", balance)
	}

	// A parent, on the other hand, may add money.
	rec = request(t, a, http.MethodPost, "/kid/"+strconv.FormatInt(k.ID, 10)+"/transaction", "papa", "parents", form)
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
	rec := request(t, a, http.MethodPost, "/kid/1/transaction", "alice", "", form)
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
	rec := request(t, a, http.MethodPost, "/kid/"+strconv.FormatInt(k.ID, 10)+"/transaction", "alice", "kids", form)
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
	rec := request(t, a, http.MethodPost, "/kid/"+strconv.FormatInt(k.ID, 10)+"/transaction", "alice", "kids", form)
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
	rec = request(t, a, http.MethodPost, "/kid/"+strconv.FormatInt(k.ID, 10)+"/transaction", "alice", "kids", form)
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
