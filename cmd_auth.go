package main

import (
	"errors"
	"flag"
	"strings"

	"github.com/javimosch/bkn/internal/auth"
	"github.com/javimosch/bkn/internal/out"
)

const authUsage = "bkn auth <user|org|member|login|refresh|me|logout|switch-org|sessions|revoke|memberships|can> ..."

func failAuth(err error) {
	switch {
	case errors.Is(err, auth.ErrUserNotFound), errors.Is(err, auth.ErrOrgNotFound):
		out.Fail(out.NotFound, "not_found", err.Error(), "bkn auth user list", "bkn auth org list")
	case errors.Is(err, auth.ErrNotAMember):
		out.Fail(out.NotFound, "not_a_member", err.Error(), "bkn auth member add <org> <user>")
	case errors.Is(err, auth.ErrEmailTaken), errors.Is(err, auth.ErrSlugTaken):
		out.Fail(out.Conflict, "already_exists", err.Error())
	case errors.Is(err, auth.ErrBadCredentials):
		out.Fail(out.NotAuthenticated, "bad_credentials", err.Error())
	case errors.Is(err, auth.ErrUserDisabled):
		out.Fail(out.NotAuthenticated, "user_disabled", err.Error(), "bkn auth user update <user> --enable")
	case errors.Is(err, auth.ErrSessionInvalid):
		out.Fail(out.NotAuthenticated, "session_invalid", err.Error(), "bkn auth login <email>")
	case errors.Is(err, auth.ErrTokenExpired):
		out.Fail(out.NotAuthenticated, "token_expired", err.Error(), "bkn auth refresh <refresh-token>")
	case errors.Is(err, auth.ErrBadToken):
		out.Fail(out.NotAuthenticated, "bad_token", err.Error())
	case errors.Is(err, auth.ErrWeakPassword), errors.Is(err, auth.ErrBadEmail),
		errors.Is(err, auth.ErrBadSlug), errors.Is(err, auth.ErrBadRole),
		errors.Is(err, auth.ErrBadGlobalRole):
		out.Fail(out.InvalidValue, "validation_error", err.Error())
	default:
		out.Fail(out.InternalError, "auth_error", err.Error())
	}
}

// password resolves a password from --password-stdin (preferred) or --password.
func password(inline string, fromStdin bool) string {
	if fromStdin {
		return strings.TrimRight(string(readAll()), "\r\n")
	}
	if inline != "" {
		// An inline password is visible in the process table and the shell
		// history. Convenient for an agent, worth saying out loud.
		out.Log("[auth] --password was passed inline; prefer --password-stdin for anything real")
	}
	return inline
}

func cmdAuth(args []string) {
	need(args, 1, authUsage)
	sub, rest := args[0], args[1:]

	conn := open()
	defer conn.Close()
	a, err := auth.New(conn, newKV(conn))
	if err != nil {
		failAuth(err)
	}

	switch sub {
	case "user":
		cmdAuthUser(a, rest)
	case "org":
		cmdAuthOrg(a, rest)
	case "member":
		cmdAuthMember(a, rest)

	case "login":
		fs := flag.NewFlagSet("auth login", flag.ExitOnError)
		pw := fs.String("password", "", "password (prefer --password-stdin)")
		stdin := fs.Bool("password-stdin", false, "read the password from stdin")
		org := fs.String("org", "", "issue the token scoped to this organization")
		pos := parseFlags(fs, rest)
		need(pos, 1, "bkn auth login <email> --password-stdin [--org <slug>]")

		tokens, err := a.Login(pos[0], password(*pw, *stdin), *org)
		if err != nil {
			failAuth(err)
		}
		out.Data(map[string]any{"tokens": tokens})

	case "refresh":
		need(rest, 1, "bkn auth refresh <refresh-token>")
		tokens, err := a.Refresh(rest[0])
		if err != nil {
			failAuth(err)
		}
		out.Data(map[string]any{"tokens": tokens})

	case "me":
		need(rest, 1, "bkn auth me <access-token>")
		u, claims, err := a.Me(rest[0])
		if err != nil {
			failAuth(err)
		}
		memberships, err := a.Memberships(u.ID)
		if err != nil {
			failAuth(err)
		}
		out.Data(map[string]any{"user": u, "claims": claims, "memberships": memberships})

	case "logout":
		need(rest, 1, "bkn auth logout <refresh-token>")
		if err := a.Logout(rest[0]); err != nil {
			failAuth(err)
		}
		// Revoking an unknown token still satisfies the caller's goal.
		out.Data(map[string]any{"logged_out": true})

	case "switch-org":
		need(rest, 2, "bkn auth switch-org <refresh-token> <org>")
		tokens, err := a.SwitchOrg(rest[0], rest[1])
		if err != nil {
			failAuth(err)
		}
		out.Data(map[string]any{"tokens": tokens})

	case "sessions":
		need(rest, 1, "bkn auth sessions <user>")
		sessions, err := a.Sessions(rest[0])
		if err != nil {
			failAuth(err)
		}
		out.Data(map[string]any{"count": len(sessions), "sessions": sessions})

	case "revoke":
		need(rest, 1, "bkn auth revoke <user>")
		n, err := a.RevokeAllSessions(rest[0])
		if err != nil {
			failAuth(err)
		}
		out.Data(map[string]any{"revoked": n})

	case "memberships":
		need(rest, 1, "bkn auth memberships <user>")
		memberships, err := a.Memberships(rest[0])
		if err != nil {
			failAuth(err)
		}
		out.Data(map[string]any{"count": len(memberships), "memberships": memberships})

	case "can":
		need(rest, 3, "bkn auth can <user> <org> <owner|admin|member>")
		allowed, err := a.Can(rest[0], rest[1], rest[2])
		if err != nil {
			failAuth(err)
		}
		// Exit code carries the answer too, so a shell can branch on it.
		if !allowed {
			out.Fail(out.NotAuthenticated, "insufficient_role",
				rest[0]+" does not hold "+rest[2]+" or above in "+rest[1])
		}
		out.Data(map[string]any{"allowed": true, "user": rest[0], "org": rest[1], "min_role": rest[2]})

	default:
		out.Fail(out.InvalidArguments, "unknown_command", "unknown auth subcommand "+sub, "usage: "+authUsage)
	}
}

func cmdAuthUser(a *auth.Auth, args []string) {
	need(args, 1, "bkn auth user <create|list|show|update|delete> ...")
	sub, rest := args[0], args[1:]

	switch sub {
	case "create":
		fs := flag.NewFlagSet("auth user create", flag.ExitOnError)
		pw := fs.String("password", "", "password (prefer --password-stdin)")
		stdin := fs.Bool("password-stdin", false, "read the password from stdin")
		name := fs.String("name", "", "display name")
		role := fs.String("role", auth.RoleUser, strings.Join(auth.GlobalRoles(), "|"))
		pos := parseFlags(fs, rest)
		need(pos, 1, "bkn auth user create <email> --password-stdin [--name N] [--role user|admin]")

		u, err := a.CreateUser(pos[0], password(*pw, *stdin), *name, *role)
		if err != nil {
			failAuth(err)
		}
		out.Data(map[string]any{"user": u})

	case "list":
		fs := flag.NewFlagSet("auth user list", flag.ExitOnError)
		limit := fs.Int("limit", 50, "maximum users")
		offset := fs.Int("offset", 0, "users to skip")
		_ = parseFlags(fs, rest)
		users, err := a.ListUsers(*limit, *offset)
		if err != nil {
			failAuth(err)
		}
		out.Data(map[string]any{"count": len(users), "users": users})

	case "show":
		need(rest, 1, "bkn auth user show <email|id>")
		u, err := a.FindUser(rest[0])
		if err != nil {
			failAuth(err)
		}
		memberships, err := a.Memberships(u.ID)
		if err != nil {
			failAuth(err)
		}
		out.Data(map[string]any{"user": u, "memberships": memberships})

	case "update":
		fs := flag.NewFlagSet("auth user update", flag.ExitOnError)
		name := fs.String("name", "", "replace the display name")
		role := fs.String("role", "", "replace the global role")
		pw := fs.String("password", "", "replace the password")
		stdin := fs.Bool("password-stdin", false, "read the new password from stdin")
		enable := fs.Bool("enable", false, "re-enable the account")
		disable := fs.Bool("disable", false, "disable the account and end its sessions")
		pos := parseFlags(fs, rest)
		need(pos, 1, "bkn auth user update <email|id> [--name N] [--role R] [--password-stdin] [--enable|--disable]")
		if *enable && *disable {
			out.Fail(out.InvalidArguments, "conflicting_flags", "--enable and --disable are mutually exclusive")
		}

		var namePtr, rolePtr, pwPtr *string
		var disabledPtr *bool
		fs.Visit(func(f *flag.Flag) {
			switch f.Name {
			case "name":
				namePtr = name
			case "role":
				rolePtr = role
			case "password":
				p := password(*pw, false)
				pwPtr = &p
			case "enable":
				no := false
				disabledPtr = &no
			case "disable":
				yes := true
				disabledPtr = &yes
			}
		})
		if *stdin {
			p := password("", true)
			pwPtr = &p
		}

		u, err := a.UpdateUser(pos[0], namePtr, rolePtr, pwPtr, disabledPtr)
		if err != nil {
			failAuth(err)
		}
		out.Data(map[string]any{"user": u})

	case "delete":
		need(rest, 1, "bkn auth user delete <email|id>")
		if err := a.DeleteUser(rest[0]); err != nil {
			failAuth(err)
		}
		out.Data(map[string]any{"deleted": rest[0]})

	default:
		out.Fail(out.InvalidArguments, "unknown_command", "unknown auth user subcommand "+sub)
	}
}

func cmdAuthOrg(a *auth.Auth, args []string) {
	need(args, 1, "bkn auth org <create|list|show|delete> ...")
	sub, rest := args[0], args[1:]

	switch sub {
	case "create":
		fs := flag.NewFlagSet("auth org create", flag.ExitOnError)
		name := fs.String("name", "", "display name (defaults to the slug)")
		pos := parseFlags(fs, rest)
		need(pos, 1, "bkn auth org create <slug> [--name N]")
		o, err := a.CreateOrg(pos[0], *name)
		if err != nil {
			failAuth(err)
		}
		out.Data(map[string]any{"org": o})

	case "list":
		orgs, err := a.ListOrgs()
		if err != nil {
			failAuth(err)
		}
		out.Data(map[string]any{"count": len(orgs), "orgs": orgs})

	case "show":
		need(rest, 1, "bkn auth org show <slug|id>")
		o, err := a.FindOrg(rest[0])
		if err != nil {
			failAuth(err)
		}
		members, err := a.Members(o.ID)
		if err != nil {
			failAuth(err)
		}
		out.Data(map[string]any{"org": o, "members": members})

	case "delete":
		need(rest, 1, "bkn auth org delete <slug|id>")
		if err := a.DeleteOrg(rest[0]); err != nil {
			failAuth(err)
		}
		out.Data(map[string]any{"deleted": rest[0]})

	default:
		out.Fail(out.InvalidArguments, "unknown_command", "unknown auth org subcommand "+sub)
	}
}

func cmdAuthMember(a *auth.Auth, args []string) {
	need(args, 1, "bkn auth member <add|remove|list> ...")
	sub, rest := args[0], args[1:]

	switch sub {
	case "add":
		fs := flag.NewFlagSet("auth member add", flag.ExitOnError)
		role := fs.String("role", auth.OrgMember, strings.Join(auth.OrgRoles(), "|"))
		pos := parseFlags(fs, rest)
		need(pos, 2, "bkn auth member add <org> <user> [--role owner|admin|member]")
		m, err := a.AddMember(pos[0], pos[1], *role)
		if err != nil {
			failAuth(err)
		}
		out.Data(map[string]any{"membership": m})

	case "remove":
		need(rest, 2, "bkn auth member remove <org> <user>")
		if err := a.RemoveMember(rest[0], rest[1]); err != nil {
			failAuth(err)
		}
		out.Data(map[string]any{"removed": rest[1], "org": rest[0]})

	case "list":
		need(rest, 1, "bkn auth member list <org>")
		members, err := a.Members(rest[0])
		if err != nil {
			failAuth(err)
		}
		out.Data(map[string]any{"count": len(members), "members": members})

	default:
		out.Fail(out.InvalidArguments, "unknown_command", "unknown auth member subcommand "+sub)
	}
}
