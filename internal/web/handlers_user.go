package web

import (
	"errors"
	"net/http"
	"strings"

	"github.com/PaulOh5/flexctl/internal/users"
)

// MaxUsers caps the per-host user count for Phase 1: a single GPU box
// is sized for ~5 people. Past that the salesperson should be selling
// a second box, not making the existing one fight harder. Eng review
// #9 (6th-user policy: reject with a clear message).
const MaxUsers = 5

// GET / — dashboard with env list + GPU availability.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	s.withUser(func(w http.ResponseWriter, r *http.Request) {
		u := currentUser(r)
		all := s.listUsers(r)

		if u == nil {
			if len(all) == 0 {
				http.Redirect(w, r, "/users/new", http.StatusSeeOther)
				return
			}
			http.Redirect(w, r, "/users", http.StatusSeeOther)
			return
		}

		s.renderDashboard(w, r, u, all, nil)
	})(w, r)
}

// GET /users — user picker. Even with cookie set, we let the user
// switch identities here.
func (s *Server) handleUserSelect(w http.ResponseWriter, r *http.Request) {
	s.withUser(func(w http.ResponseWriter, r *http.Request) {
		all := s.listUsers(r)
		if len(all) == 0 {
			http.Redirect(w, r, "/users/new", http.StatusSeeOther)
			return
		}
		s.render(w, "user_select", pageData{
			Title:       "Pick user",
			CurrentUser: currentUser(r),
			Users:       all,
			Data: map[string]any{
				"AtCapacity": len(all) >= MaxUsers,
				"MaxUsers":   MaxUsers,
			},
		})
	})(w, r)
}

// POST /users/select — set the cookie to the chosen email.
func (s *Server) handleUserSelectSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	email := strings.TrimSpace(r.PostForm.Get("email"))
	if email == "" {
		http.Redirect(w, r, "/users", http.StatusSeeOther)
		return
	}
	// Verify the user exists; the cookie stores email not id so a
	// stale cookie after deletion fails closed at withUser time.
	if _, err := s.users.GetByEmail(r.Context(), email); err != nil {
		http.Error(w, "unknown user", http.StatusBadRequest)
		return
	}
	setUserCookie(w, email)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// POST /users/sign-out — clear the cookie.
func (s *Server) handleSignOut(w http.ResponseWriter, r *http.Request) {
	clearUserCookie(w)
	http.Redirect(w, r, "/users", http.StatusSeeOther)
}

// GET /users/new — creation form.
func (s *Server) handleUserNew(w http.ResponseWriter, r *http.Request) {
	all := s.listUsers(r)
	atCap := len(all) >= MaxUsers
	s.render(w, "user_new", pageData{
		Title: "Add user",
		Users: all,
		Data: map[string]any{
			"AtCapacity": atCap,
			"MaxUsers":   MaxUsers,
		},
	})
}

// POST /users — create + auto-select.
func (s *Server) handleUserCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	email := strings.TrimSpace(r.PostForm.Get("email"))
	if !looksLikeEmail(email) {
		s.renderUserNewError(w, r, email, "이메일 형식이 올바르지 않습니다.")
		return
	}

	all := s.listUsers(r)
	if len(all) >= MaxUsers {
		s.renderUserNewError(w, r, email,
			"이 박스는 최대 5명까지 권장됩니다. 더 필요하면 다음 박스로 분리하세요.")
		return
	}

	u, err := s.users.Create(r.Context(), email)
	switch {
	case errors.Is(err, users.ErrEmailTaken):
		s.renderUserNewError(w, r, email, "이미 등록된 이메일입니다.")
		return
	case err != nil:
		s.log.Error("user create", "err", err)
		http.Error(w, "서버 오류, 잠시 후 다시", http.StatusInternalServerError)
		return
	}

	setUserCookie(w, u.Email)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) renderUserNewError(w http.ResponseWriter, r *http.Request, email, msg string) {
	all := s.listUsers(r)
	s.render(w, "user_new", pageData{
		Title: "Add user",
		Users: all,
		Flash: &flash{Kind: "error", Msg: msg},
		Data: map[string]any{
			"AtCapacity": len(all) >= MaxUsers,
			"MaxUsers":   MaxUsers,
			"Email":      email,
		},
	})
}

// looksLikeEmail is intentionally lax: a real RFC 5322 check is its own
// novel. We just want to fail obvious typos before hitting the DB.
func looksLikeEmail(s string) bool {
	if len(s) < 3 || len(s) > 254 {
		return false
	}
	at := strings.IndexByte(s, '@')
	if at <= 0 || at == len(s)-1 {
		return false
	}
	if strings.ContainsAny(s, " \t\n\r") {
		return false
	}
	return true
}

func setUserCookie(w http.ResponseWriter, email string) {
	http.SetCookie(w, &http.Cookie{
		Name:     userCookie,
		Value:    email,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   30 * 24 * 60 * 60, // 30 days
	})
}

func clearUserCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     userCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}
