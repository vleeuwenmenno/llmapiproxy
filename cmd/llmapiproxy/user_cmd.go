package main

import (
	"fmt"
	"strings"
	"syscall"

	"github.com/menno/llmapiproxy/internal/users"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var userCmd = &cobra.Command{
	Use:   "user",
	Short: "Manage web UI users",
	Long:  "Create, list, delete, and manage passwords for web UI users. These users authenticate against the /ui/* dashboard and settings pages.",
}

var userAddCmd = &cobra.Command{
	Use:   "add <username>",
	Short: "Create a new user",
	Long:  "Create a new web UI user. You will be prompted for a password (typed twice for confirmation).",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		username := args[0]
		dbPath, _ := cmd.Flags().GetString("users-db")
		passwordFlag, _ := cmd.Flags().GetString("password")

		var password string
		if passwordFlag != "" {
			password = passwordFlag
		} else {
			var err error
			password, err = promptPasswordWithConfirm("Enter password: ", "Confirm password: ")
			if err != nil {
				return err
			}
		}

		if err := validatePassword(password); err != nil {
			return err
		}

		store, err := users.OpenUserStore(dbPath)
		if err != nil {
			return fmt.Errorf("opening users database: %w", err)
		}
		defer store.Close()

		if err := store.CreateUser(username, password); err != nil {
			return fmt.Errorf("creating user: %w", err)
		}

		fmt.Printf("User %q created successfully.\n", username)
		return nil
	},
}

var userListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all users",
	Long:  "Display all registered web UI users with their roles and creation dates.",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		dbPath, _ := cmd.Flags().GetString("users-db")

		store, err := users.OpenUserStore(dbPath)
		if err != nil {
			return fmt.Errorf("opening users database: %w", err)
		}
		defer store.Close()

		userList, err := store.ListUsers()
		if err != nil {
			return fmt.Errorf("listing users: %w", err)
		}

		if len(userList) == 0 {
			fmt.Println("No users found.")
			return nil
		}

		fmt.Printf("%-20s %-10s %s\n", "USERNAME", "ROLE", "CREATED")
		for _, u := range userList {
			fmt.Printf("%-20s %-10s %s\n", u.Username, u.Role, u.CreatedAt.Format("2006-01-02 15:04:05"))
		}
		return nil
	},
}

var userDeleteCmd = &cobra.Command{
	Use:     "remove <username>",
	Short:   "Remove a user",
	Long:    "Remove a web UI user by username.",
	Args:    cobra.ExactArgs(1),
	Aliases: []string{"delete", "rm"},
	RunE: func(cmd *cobra.Command, args []string) error {
		username := args[0]
		dbPath, _ := cmd.Flags().GetString("users-db")

		store, err := users.OpenUserStore(dbPath)
		if err != nil {
			return fmt.Errorf("opening users database: %w", err)
		}
		defer store.Close()

		if err := store.DeleteUser(username); err != nil {
			return err
		}

		fmt.Printf("User %q removed.\n", username)
		return nil
	},
}

var userPasswdCmd = &cobra.Command{
	Use:   "passwd <username>",
	Short: "Change a user's password",
	Long:  "Change the password for an existing web UI user. You will be prompted for the new password (typed twice for confirmation).",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		username := args[0]
		dbPath, _ := cmd.Flags().GetString("users-db")
		passwordFlag, _ := cmd.Flags().GetString("password")

		var password string
		if passwordFlag != "" {
			password = passwordFlag
		} else {
			var err error
			password, err = promptPasswordWithConfirm("Enter new password: ", "Confirm new password: ")
			if err != nil {
				return err
			}
		}

		if err := validatePassword(password); err != nil {
			return err
		}

		store, err := users.OpenUserStore(dbPath)
		if err != nil {
			return fmt.Errorf("opening users database: %w", err)
		}
		defer store.Close()

		if err := store.ChangePassword(username, password); err != nil {
			return err
		}

		fmt.Printf("Password for %q updated.\n", username)
		return nil
	},
}

func init() {
	// Persistent flags available on all user subcommands.
	userCmd.PersistentFlags().String("users-db", "data/users.db", "Path to users database")

	// --password flag on add and passwd for scripting/non-interactive use.
	userAddCmd.Flags().String("password", "", "Set password directly (skips prompt, no confirmation)")
	userPasswdCmd.Flags().String("password", "", "Set password directly (skips prompt, no confirmation)")

	userCmd.AddCommand(userAddCmd)
	userCmd.AddCommand(userListCmd)
	userCmd.AddCommand(userDeleteCmd)
	userCmd.AddCommand(userPasswdCmd)
}

// promptPasswordWithConfirm prompts the user twice and ensures both entries match.
func promptPasswordWithConfirm(prompt1, prompt2 string) (string, error) {
	fmt.Print(prompt1)
	pw1, err := term.ReadPassword(int(syscall.Stdin))
	fmt.Println()
	if err != nil {
		return "", fmt.Errorf("reading password: %w", err)
	}

	fmt.Print(prompt2)
	pw2, err := term.ReadPassword(int(syscall.Stdin))
	fmt.Println()
	if err != nil {
		return "", fmt.Errorf("reading password confirmation: %w", err)
	}

	s1 := strings.TrimSpace(string(pw1))
	s2 := strings.TrimSpace(string(pw2))

	if s1 != s2 {
		return "", fmt.Errorf("passwords do not match")
	}

	return s1, nil
}

// validatePassword checks minimum password requirements.
func validatePassword(password string) error {
	if len(password) < 8 {
		return fmt.Errorf("password must be at least 8 characters")
	}
	return nil
}
