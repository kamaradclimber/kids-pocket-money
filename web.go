package main

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

//go:embed templates/*.html
var templateFiles embed.FS

const csrfCookieName = "kids_pocket_money_csrf"

// maxAmountCents is the largest absolute amount a transaction may carry.
const maxAmountCents = 1_000_000_00 // 10 000 €

type app struct {
	cfg       config
	store     *store
	templates *template.Template
}

type pageData struct {
	User      string
	Email     string
	Error     string
	Success   string
	CSRFToken string
	Nonce     string
	IsAdmin   bool

	// kid dashboard (GET /)
	Kid          *kidOverview
	Spent        int64
	Transactions []transaction

	// parent dashboard (GET /)
	Kids []kidOverview

	// kid detail (GET /enfant/{username})
	Detail *kidOverview
}

func newApp(cfg config, st *store) (*app, error) {
	funcs := template.FuncMap{
		"euro":      euro,
		"date":      func(t time.Time) string { return t.Format("02/01/2006") },
		"niceDate":  niceDate,
		"monthName": monthName,
	}
	templates, err := template.New("").Funcs(funcs).ParseFS(templateFiles, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	return &app{cfg: cfg, store: st, templates: templates}, nil
}

// euro formats integer cents as "12,50 €".
func euro(cents int64) string {
	sign := ""
	if cents < 0 {
		sign = "-"
		cents = -cents
	}
	return fmt.Sprintf("%s%d,%02d €", sign, cents/100, cents%100)
}

// niceDate renders an operation date ("2006-01-02") in French day-first
// order; anything unparseable (should not happen) is shown as-is.
func niceDate(date string) string {
	if t, err := time.Parse("2006-01-02", date); err == nil {
		return t.Format("02/01/2006")
	}
	return date
}

func monthName(month string) string {
	t, err := time.Parse("2006-01", month)
	if err != nil {
		return month
	}
	// French month names, first letter capitalized.
	names := map[time.Month]string{
		time.January: "janvier", time.February: "février", time.March: "mars",
		time.April: "avril", time.May: "mai", time.June: "juin",
		time.July: "juillet", time.August: "août", time.September: "septembre",
		time.October: "octobre", time.November: "novembre", time.December: "décembre",
	}
	return names[t.Month()] + " " + strconv.Itoa(t.Year())
}

func (a *app) routes() http.Handler {
	mux := http.NewServeMux()
	// Probes stay public: no Remote-User required.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	app := http.NewServeMux()
	app.HandleFunc("GET /", a.handleIndex)
	app.HandleFunc("GET /enfant/{username}", a.handleKid)
	app.HandleFunc("POST /kid", a.handleCreateKid)
	app.HandleFunc("POST /enfant/{username}/monthly", a.handleMonthly)
	app.HandleFunc("POST /enfant/{username}/transaction", a.handleTransaction)
	app.HandleFunc("POST /transaction/{id}/delete", a.handleDeleteTransaction)
	// Everything else goes through the global access rule.
	mux.Handle("/", a.requireAuth(app))
	return a.withHeaders(mux)
}

// requireAuth denies access to the whole app unless the request carries a
// Remote-User and belongs to either the parents or the kids group. Health
// probes stay public. Parents get the family-wide view, kids their own.
func (a *app) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if remoteUser(r) == "" {
			http.Error(w, "Authentification requise.", http.StatusUnauthorized)
			return
		}
		if !hasGroup(r, a.cfg.AdminGroup) && !hasGroup(r, a.cfg.KidsGroup) {
			http.Error(w, "Accès réservé aux membres des groupes des parents ou des enfants.", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *app) withHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

func remoteUser(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get("Remote-User"))
}

// remoteName returns the display name of the authenticated user, as
// provided by authelia in the Remote-Name header. Falls back to the
// username when the header is absent.
func remoteName(r *http.Request) string {
	if name := strings.TrimSpace(r.Header.Get("Remote-Name")); name != "" {
		if len(name) > 200 {
			name = name[:200]
		}
		return name
	}
	return remoteUser(r)
}

func hasGroup(r *http.Request, wanted string) bool {
	for _, value := range r.Header.Values("Remote-Groups") {
		for _, group := range strings.Split(value, ",") {
			if strings.TrimSpace(group) == wanted {
				return true
			}
		}
	}
	return false
}

func (a *app) notFound(w http.ResponseWriter, r *http.Request) {
	data := pageData{User: remoteUser(r), Nonce: newNonce()}
	a.render(w, r, http.StatusNotFound, "404.html", data)
}

// GET /: family dashboard for parents (all kids at once), personal
// dashboard for kids (own balance and history only).
func (a *app) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		a.notFound(w, r)
		return
	}
	user := remoteUser(r)
	if user == "" {
		http.Error(w, "Authentification requise.", http.StatusUnauthorized)
		return
	}
	isAdmin := hasGroup(r, a.cfg.AdminGroup)
	isKid := hasGroup(r, a.cfg.KidsGroup)

	if _, err := a.store.ensureMonthlyAllowances(time.Now()); err != nil {
		slog.Error("ensure allowances", "error", err)
	}

	data := a.basePageData(w, r, user, isAdmin)

	// Parents see every kid's money at once.
	if isAdmin {
		data.Kids, _ = a.store.overviews()
		a.render(w, r, http.StatusOK, "admin.html", data)
		return
	}

	// Kids only ever see their own account.
	if !isKid {
		http.Error(w, "Réservé aux membres du groupe des parents ou des enfants.", http.StatusForbidden)
		return
	}
	k, found, err := a.store.kidByUsername(user)
	if err != nil {
		http.Error(w, "Erreur interne.", http.StatusInternalServerError)
		return
	}
	if !found {
		http.Error(w, "Ce compte n'est pas inscrit. Demande à un parent de le configurer.", http.StatusForbidden)
		return
	}
	overview, err := a.store.overview(k)
	if err != nil {
		http.Error(w, "Erreur interne.", http.StatusInternalServerError)
		return
	}
	data.Kid = &overview
	data.Transactions, _ = a.store.listTransactions(k.ID)
	var spent int64
	for _, tx := range data.Transactions {
		if tx.Amount < 0 {
			spent -= tx.Amount
		}
	}
	data.Spent = spent
	a.render(w, r, http.StatusOK, "index.html", data)
}

// resolveKid looks up the kid named by the {username} path value. It
// answers 404 itself when the username is unknown.
func (a *app) resolveKid(w http.ResponseWriter, r *http.Request) (kid, bool) {
	username := r.PathValue("username")
	k, found, err := a.store.kidByUsername(username)
	if err != nil {
		http.Error(w, "Erreur interne.", http.StatusInternalServerError)
		return kid{}, false
	}
	if !found {
		a.notFound(w, r)
		return kid{}, false
	}
	return k, true
}

// GET /enfant/{username}: history + parent forms for one kid. Parents only:
// a kid's own history is on their dashboard, and kids must not see each
// other's money.
func (a *app) handleKid(w http.ResponseWriter, r *http.Request) {
	if !hasGroup(r, a.cfg.AdminGroup) {
		// Kid detail pages are parent-only; kids use their own dashboard.
		a.notFound(w, r)
		return
	}
	user := remoteUser(r)
	isAdmin := true
	if _, err := a.store.ensureMonthlyAllowances(time.Now()); err != nil {
		slog.Error("ensure allowances", "error", err)
	}
	k, ok := a.resolveKid(w, r)
	if !ok {
		return
	}
	data := a.basePageData(w, r, user, isAdmin)
	overview, err := a.store.overview(k)
	if err != nil {
		http.Error(w, "Erreur interne.", http.StatusInternalServerError)
		return
	}
	data.Detail = &overview
	data.Transactions, _ = a.store.listTransactions(k.ID)
	a.render(w, r, http.StatusOK, "kid.html", data)
}

// POST /kid: create a kid (parents only).
func (a *app) handleCreateKid(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	user, isAdmin := a.authedAdmin(w, r)
	if user == "" || !isAdmin {
		return
	}
	if !validCSRF(r) {
		a.renderAdminError(w, r, user, isAdmin, http.StatusForbidden, "Jeton de formulaire invalide. Recharge la page et réessaie.")
		return
	}
	username := strings.TrimSpace(r.FormValue("username"))
	name := strings.TrimSpace(r.FormValue("name"))
	if username == "" || name == "" {
		a.renderAdminError(w, r, user, isAdmin, http.StatusBadRequest, "Le nom d'utilisateur et le prénom sont obligatoires.")
		return
	}
	amount, err := parseAmount(r.FormValue("monthly_amount"))
	if err != nil {
		a.renderAdminError(w, r, user, isAdmin, http.StatusBadRequest, err.Error())
		return
	}
	if amount < 0 || amount > maxAmountCents {
		a.renderAdminError(w, r, user, isAdmin, http.StatusBadRequest, "Le montant mensuel doit être entre 0 et 10 000 €.")
		return
	}
	startMonth := strings.TrimSpace(r.FormValue("start_month"))
	if startMonth == "" {
		startMonth = time.Now().Format("2006-01")
	}
	if !validMonth(startMonth) {
		a.renderAdminError(w, r, user, isAdmin, http.StatusBadRequest, "Le mois de début doit ressembler à 2025-11.")
		return
	}
	if startMonth > time.Now().Format("2006-01") {
		a.renderAdminError(w, r, user, isAdmin, http.StatusBadRequest, "Le mois de début ne peut pas être dans le futur.")
		return
	}
	k, err := a.store.createKid(username, name, amount, startMonth, user, time.Now())
	if err != nil {
		a.renderAdminError(w, r, user, isAdmin, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := a.store.ensureMonthlyAllowances(time.Now()); err != nil {
		slog.Error("ensure allowances", "error", err)
	}
	slog.Info("kid created", "kid", k.Name, "username", k.Username, "by", user)
	http.Redirect(w, r, "/enfant/"+url.PathEscape(k.Username), http.StatusSeeOther)
}

// POST /enfant/{username}/monthly: change a kid's monthly amount (parents only).
func (a *app) handleMonthly(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	user, isAdmin := a.authedAdmin(w, r)
	if user == "" || !isAdmin {
		return
	}
	if !validCSRF(r) {
		a.renderAdminError(w, r, user, isAdmin, http.StatusForbidden, "Jeton de formulaire invalide. Recharge la page et réessaie.")
		return
	}
	k, ok := a.resolveKid(w, r)
	if !ok {
		return
	}
	amount, err := parseAmount(r.FormValue("monthly_amount"))
	if err != nil {
		a.renderKidError(w, r, user, k, http.StatusBadRequest, err.Error())
		return
	}
	if amount < 0 || amount > maxAmountCents {
		a.renderKidError(w, r, user, k, http.StatusBadRequest, "Le montant mensuel doit être entre 0 et 10 000 €.")
		return
	}
	if err := a.store.updateMonthlyAmount(k.ID, amount); err != nil {
		slog.Error("update monthly amount", "error", err)
		a.renderKidError(w, r, user, k, http.StatusInternalServerError, "Erreur à l'enregistrement.")
		return
	}
	slog.Info("monthly amount updated", "kid", k.Username, "amount", amount, "by", user)
	http.Redirect(w, r, "/enfant/"+url.PathEscape(k.Username), http.StatusSeeOther)
}

// POST /enfant/{username}/transaction: add a manual transaction. Parents may
// act on any kid; a kid may only act on their own account.
func (a *app) handleTransaction(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	user := remoteUser(r)
	if user == "" {
		http.Error(w, "Authentification requise.", http.StatusUnauthorized)
		return
	}
	isAdmin := hasGroup(r, a.cfg.AdminGroup)
	isKid := hasGroup(r, a.cfg.KidsGroup)
	if !isAdmin && !isKid {
		http.Error(w, "Réservé aux parents et aux enfants.", http.StatusForbidden)
		return
	}
	if !validCSRF(r) {
		a.renderActionError(w, r, user, isAdmin, http.StatusForbidden, "Jeton de formulaire invalide. Recharge la page et réessaie.")
		return
	}
	k, ok := a.resolveKid(w, r)
	if !ok {
		return
	}
	// A kid may only act on their own account; a parent may act on any kid.
	// Answer 404 rather than 403 so as not to leak other kids' existence.
	if !isAdmin && k.Username != user {
		a.notFound(w, r)
		return
	}
	amount, err := parseAmount(r.FormValue("amount"))
	if err != nil {
		a.renderActionError(w, r, user, isAdmin, http.StatusBadRequest, err.Error())
		return
	}
	// Kids may only record spends: adding money to an account is a
	// parent-only action (the kid templates don't even offer the choice).
	kind := r.FormValue("kind")
	if !isAdmin && kind != "spend" && kind != "" {
		a.renderActionError(w, r, user, isAdmin, http.StatusForbidden, "Seuls les parents peuvent ajouter de l'argent à un compte.")
		return
	}
	switch kind {
	case "add":
		if amount < 0 {
			amount = -amount
		}
	case "spend", "":
		if amount > 0 {
			amount = -amount
		}
	default:
		a.renderActionError(w, r, user, isAdmin, http.StatusBadRequest, "Type d'opération invalide.")
		return
	}
	if amount == 0 {
		a.renderActionError(w, r, user, isAdmin, http.StatusBadRequest, "Le montant ne peut pas être nul.")
		return
	}
	if amount < -maxAmountCents || amount > maxAmountCents {
		a.renderActionError(w, r, user, isAdmin, http.StatusBadRequest, "Le montant doit être entre -10 000 € et 10 000 €.")
		return
	}
	label := strings.TrimSpace(r.FormValue("label"))
	if label == "" {
		a.renderActionError(w, r, user, isAdmin, http.StatusBadRequest, "Décris l'opération (ex : « achat d'un jeu »).")
		return
	}
	if len(label) > 200 {
		label = label[:200]
	}
	// Optional operation date (empty = today). HTML date inputs submit
	// "YYYY-MM-DD".
	date := time.Now()
	if raw := strings.TrimSpace(r.FormValue("date")); raw != "" {
		parsed, err := time.Parse("2006-01-02", raw)
		if err != nil {
			a.renderActionError(w, r, user, isAdmin, http.StatusBadRequest, "Date invalide : utilise le calendrier ou laisse vide pour aujourd'hui.")
			return
		}
		// Refuse far-fetched dates (guards the month derivation and the
		// ordering) without forbidding the odd future purchase.
		if parsed.Year() < 2000 || parsed.Year() > time.Now().Year()+1 {
			a.renderActionError(w, r, user, isAdmin, http.StatusBadRequest, "Date hors limites.")
			return
		}
		date = parsed
	}
	if _, err := a.store.addTransaction(k.ID, amount, label, remoteName(r), user, date, time.Now()); err != nil {
		slog.Error("add transaction", "error", err)
		a.renderActionError(w, r, user, isAdmin, http.StatusInternalServerError, "Erreur à l'enregistrement.")
		return
	}
	slog.Info("transaction added", "kid", k.Name, "amount", amount, "label", label, "by", user)
	a.redirectAfterAction(w, r, isAdmin, k)
}

// POST /transaction/{id}/delete: delete a manual transaction (parents only).
func (a *app) handleDeleteTransaction(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	user, isAdmin := a.authedAdmin(w, r)
	if user == "" || !isAdmin {
		return
	}
	if !validCSRF(r) {
		a.renderAdminError(w, r, user, isAdmin, http.StatusForbidden, "Jeton de formulaire invalide. Recharge la page et réessaie.")
		return
	}
	txID := parseID(r, "id")
	kidID, err := a.store.kidIDOfTransaction(txID)
	if err != nil {
		a.notFound(w, r)
		return
	}
	k, err := a.store.getKid(kidID)
	if errors.Is(err, errKidNotFound) {
		a.notFound(w, r)
		return
	} else if err != nil {
		http.Error(w, "Erreur interne.", http.StatusInternalServerError)
		return
	}
	if err := a.store.deleteTransaction(txID); err != nil {
		slog.Error("delete transaction", "error", err)
		http.Error(w, "Impossible de supprimer cette opération.", http.StatusBadRequest)
		return
	}
	slog.Info("transaction deleted", "transaction_id", txID, "by", user)
	http.Redirect(w, r, "/enfant/"+url.PathEscape(k.Username), http.StatusSeeOther)
}

func (a *app) authedAdmin(w http.ResponseWriter, r *http.Request) (user string, isAdmin bool) {
	user = remoteUser(r)
	if user == "" {
		http.Error(w, "Authentification requise.", http.StatusUnauthorized)
		return "", false
	}
	isAdmin = hasGroup(r, a.cfg.AdminGroup)
	if !isAdmin {
		http.Error(w, "Réservé aux parents.", http.StatusForbidden)
	}
	return user, isAdmin
}

func parseID(r *http.Request, name string) int64 {
	id, _ := strconv.ParseInt(r.PathValue(name), 10, 64)
	return id
}

// redirectAfterAction sends a kid back to / and a parent to the kid page.
func (a *app) redirectAfterAction(w http.ResponseWriter, r *http.Request, isAdmin bool, k kid) {
	if isAdmin {
		http.Redirect(w, r, "/enfant/"+url.PathEscape(k.Username), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// renderActionError re-renders the page the user came from with an error
// message (kid dashboard or kid detail, depending on role).
func (a *app) renderActionError(w http.ResponseWriter, r *http.Request, user string, isAdmin bool, status int, message string) {
	k, found, err := a.store.kidByUsername(r.PathValue("username"))
	if isAdmin {
		if err == nil && found {
			a.renderKidError(w, r, user, k, status, message)
		} else {
			a.renderAdminError(w, r, user, isAdmin, status, message)
		}
		return
	}
	a.renderKidDashboardError(w, r, user, status, message)
}

func (a *app) renderKidError(w http.ResponseWriter, r *http.Request, user string, k kid, status int, message string) {
	data := a.basePageData(w, r, user, true)
	overview, err := a.store.overview(k)
	if err == nil {
		data.Detail = &overview
		data.Transactions, _ = a.store.listTransactions(k.ID)
	}
	data.Error = message
	a.render(w, r, status, "kid.html", data)
}

func (a *app) renderKidDashboardError(w http.ResponseWriter, r *http.Request, user string, status int, message string) {
	data := a.basePageData(w, r, user, false)
	if k, found, err := a.store.kidByUsername(user); err == nil && found {
		overview, _ := a.store.overview(k)
		data.Kid = &overview
		data.Transactions, _ = a.store.listTransactions(k.ID)
	}
	data.Error = message
	a.render(w, r, status, "index.html", data)
}

func (a *app) renderAdminError(w http.ResponseWriter, r *http.Request, user string, isAdmin bool, status int, message string) {
	data := a.basePageData(w, r, user, isAdmin)
	data.Kids, _ = a.store.overviews()
	data.Error = message
	a.render(w, r, status, "admin.html", data)
}

func (a *app) basePageData(w http.ResponseWriter, r *http.Request, user string, isAdmin bool) pageData {
	csrf, err := getOrCreateCSRF(w, r, a.cfg.SecureCookie)
	if err != nil {
		slog.Error("csrf", "error", err)
	}
	return pageData{
		User:      user,
		Email:     strings.TrimSpace(r.Header.Get("Remote-Email")),
		CSRFToken: csrf,
		Nonce:     newNonce(),
		IsAdmin:   isAdmin,
	}
}

func (a *app) render(w http.ResponseWriter, r *http.Request, status int, templateName string, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", fmt.Sprintf(
		"default-src 'none'; style-src 'unsafe-inline'; script-src 'nonce-%s'; connect-src 'self'; img-src 'self' data:; form-action 'self'; base-uri 'none'; frame-ancestors 'none'",
		data.Nonce,
	))
	w.WriteHeader(status)
	if err := a.templates.ExecuteTemplate(w, templateName, data); err != nil {
		slog.Error("render template", "template", templateName, "error", err)
	}
}

// parseAmount parses a user-entered euro amount ("12,50", "12.5", "-3")
// into integer cents. Empty input is an error.
func parseAmount(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, fmt.Errorf("Saisis un montant.")
	}
	raw = strings.ReplaceAll(raw, " ", "")
	raw = strings.Replace(raw, ",", ".", 1)
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, fmt.Errorf("Montant invalide.")
	}
	if math.Abs(value) > 9e13 { // way beyond maxAmountCents, avoids int64 overflow
		return 0, fmt.Errorf("Montant trop grand.")
	}
	return int64(math.Round(value * 100)), nil
}

// validMonth reports whether s is a "YYYY-MM" string.
func validMonth(s string) bool {
	if len(s) != 7 || s[4] != '-' {
		return false
	}
	_, err := time.Parse("2006-01", s)
	return err == nil
}

func newNonce() string {
	random := make([]byte, 18)
	if _, err := rand.Read(random); err != nil {
		panic(fmt.Sprintf("generate nonce: %v", err))
	}
	return hex.EncodeToString(random)
}

func getOrCreateCSRF(w http.ResponseWriter, r *http.Request, secure bool) (string, error) {
	if cookie, err := r.Cookie(csrfCookieName); err == nil && len(cookie.Value) >= 32 {
		return cookie.Value, nil
	}
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate csrf token: %w", err)
	}
	token := hex.EncodeToString(random)
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   3600,
	})
	return token, nil
}

func validCSRF(r *http.Request) bool {
	cookie, err := r.Cookie(csrfCookieName)
	if err != nil || len(cookie.Value) < 32 {
		return false
	}
	formValue := r.FormValue("csrf_token")
	return subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(formValue)) == 1
}
