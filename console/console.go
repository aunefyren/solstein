// Package console is Solstein's command line for things that must never be
// possible through the web: adding users and resetting their passwords and
// authenticators (docs/sign-in.md). `solstein user …` runs it instead of the
// app, against the same database, while the app keeps running.
package console

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"text/tabwriter"
	"time"

	"aunefyren/solstein/auth"
	"aunefyren/solstein/database"
	"aunefyren/solstein/settings"
)

const usage = `Usage: solstein user <command> [-configdir DIR] [name]

Commands:
  list                    list the users
  add <name>              add a user, with a one-time password
  reset-password <name>   give a user a new one-time password; signs them out
  reset-mfa <name>        remove a user's authenticator (a lost phone); signs them out
  delete <name>           remove a user

A one-time password works once, within 24 hours; the user then chooses their
own. In Docker: docker exec <container> /app/solstein user add <name>
`

// Run runs `solstein user <args>` and returns the exit code.
func Run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("solstein user", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprint(stderr, usage) }
	configDir := flags.String("configdir", settings.DefaultConfigDir(getenv), "Directory holding Solstein's database.")

	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(stderr, usage)
		return 2
	}
	command := args[0]
	// Flags before or after the name: the flag package stops at the first
	// argument that isn't one, so parse again after each.
	var positional []string
	for rest := args[1:]; ; {
		if err := flags.Parse(rest); err != nil {
			return 2
		}
		if flags.NArg() == 0 {
			break
		}
		positional = append(positional, flags.Arg(0))
		rest = flags.Args()[1:]
	}
	var name string
	switch {
	case !slices.Contains([]string{"list", "add", "reset-password", "reset-mfa", "delete"}, command):
		fmt.Fprint(stderr, usage)
		return 2
	case command == "list" && len(positional) == 0:
	case command != "list" && len(positional) == 1:
		name = positional[0]
	default:
		fmt.Fprint(stderr, usage)
		return 2
	}

	// Run as whoever owns the data, so a `docker exec` as root can't leave
	// database files the app can no longer write.
	if err := runAsOwnerOf(*configDir); err != nil {
		fmt.Fprintln(stderr, "Failed to switch to the config directory's owner. Error: "+err.Error())
		return 1
	}
	store, err := database.Open(*configDir)
	if err != nil {
		fmt.Fprintln(stderr, "Failed to open the database in "+*configDir+". Error: "+err.Error())
		return 1
	}
	defer store.Close()
	service, err := auth.New(store, nil)
	if err != nil {
		fmt.Fprintln(stderr, "Failed to set up sign-in. Error: "+err.Error())
		return 1
	}

	if err := run(context.Background(), service, command, name, stdout); err != nil {
		switch {
		case errors.Is(err, database.ErrUserNotFound):
			fmt.Fprintf(stderr, "No user '%s'.\n", name)
		case errors.Is(err, database.ErrUserExists):
			fmt.Fprintf(stderr, "There is already a user '%s'.\n", name)
		case errors.Is(err, auth.ErrInvalidUsername):
			fmt.Fprintln(stderr, "Not a valid username: "+err.Error()+".")
		case errors.Is(err, errUnknownCommand):
			fmt.Fprint(stderr, usage)
			return 2
		default:
			fmt.Fprintln(stderr, "Failed. Error: "+err.Error())
		}
		return 1
	}
	return 0
}

var errUnknownCommand = errors.New("unknown command")

func run(ctx context.Context, service *auth.Service, command, name string, stdout io.Writer) error {
	switch command {
	case "list":
		users, err := service.Users(ctx)
		if err != nil {
			return err
		}
		if len(users) == 0 {
			fmt.Fprintln(stdout, "No users yet. Add one with: solstein user add <name>")
			return nil
		}
		table := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(table, "USER\tTOTP\tPASSWORD\tLAST SIGN-IN")
		for _, user := range users {
			totp, password, lastSignIn := "off", "own", "never"
			if user.TOTPEnabled {
				totp = "on"
			}
			if user.MustChangePassword {
				password = "one-time"
			}
			if user.LastSignInAt != nil {
				lastSignIn = user.LastSignInAt.Local().Format(time.DateTime)
			}
			fmt.Fprintf(table, "%s\t%s\t%s\t%s\n", user.Username, totp, password, lastSignIn)
		}
		return table.Flush()
	case "add":
		user, oneTime, err := service.AddUser(ctx, name)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Added user '%s'.\n", user.Username)
		printOneTime(stdout, oneTime)
	case "reset-password":
		oneTime, ended, err := service.ResetPassword(ctx, name)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Reset the password of '%s' and ended %d sessions. Their authenticator is unchanged.\n", name, ended)
		printOneTime(stdout, oneTime)
	case "reset-mfa":
		ended, err := service.ResetTOTP(ctx, name)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Removed the authenticator of '%s' and ended %d sessions. They sign in with their password alone until they set one up again under Account.\n", name, ended)
	case "delete":
		if err := service.DeleteUser(ctx, name); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Deleted user '%s' and their sessions.\n", name)
	default:
		return errUnknownCommand
	}
	return nil
}

func printOneTime(stdout io.Writer, oneTime string) {
	fmt.Fprintf(stdout, "One-time password: %s\nIt works once, within 24 hours, and isn't shown again; at sign-in they choose their own.\n", oneTime)
}
