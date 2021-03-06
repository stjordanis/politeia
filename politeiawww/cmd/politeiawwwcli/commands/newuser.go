package commands

import (
	"encoding/hex"
	"fmt"

	"github.com/decred/politeia/politeiawww/api/v1"
	"github.com/decred/politeia/util"
)

// Help message displayed for the command 'politeiawwwcli help newuser'
var NewUserCmdHelpMsg = `newuser "email" "username" "password" 

Create a new Politeia user. Users can be created by supplying all the arguments
below, or supplying the --random flag. If --random is used, Politeia will 
generate a random email, username and password.

Arguments:
1. email      (string, required)   Email address
2. username   (string, required)   Username 
3. password   (string, required)   Password

Request:
{
  "email":      (string)  User email
  "password":   (string)  Password
  "publickey":  (string)  Active public key
  "username":   (string)  Username
}

Response:
{
  "verificationtoken":   (string)  Server verification token
}`

type NewUserCmd struct {
	Args struct {
		Email    string `positional-arg-name:"email"`
		Username string `positional-arg-name:"username"`
		Password string `positional-arg-name:"password"`
	} `positional-args:"true"`
	Random  bool `long:"random" optional:"true" description:"Generate a random email/password for the user"`
	Paywall bool `long:"paywall" optional:"true" description:"Satisfy paywall fee using testnet faucet"`
	Verify  bool `long:"verify" optional:"true" description:"Verify the user's email address"`
	NoSave  bool `long:"nosave" optional:"true" description:"Do not save the user identity to disk"`
}

func (cmd *NewUserCmd) Execute(args []string) error {
	email := cmd.Args.Email
	username := cmd.Args.Username
	password := cmd.Args.Password

	if !cmd.Random && (email == "" || username == "" || password == "") {
		return fmt.Errorf("invalid credentials: you must either specify user " +
			"credentials (email, username, password) or use the --random flag")
	}

	// Fetch CSRF tokens
	_, err := c.Version()
	if err != nil {
		return fmt.Errorf("Version: %v", err)
	}

	// Fetch  policy for password requirements
	pr, err := c.Policy()
	if err != nil {
		return fmt.Errorf("Policy: %v", err)
	}

	// Create new user credentials if required
	if cmd.Random {
		b, err := util.Random(int(pr.MinPasswordLength))
		if err != nil {
			return err
		}

		email = hex.EncodeToString(b) + "@example.com"
		username = hex.EncodeToString(b)
		password = hex.EncodeToString(b)
	}

	// Validate password
	if uint(len(password)) < pr.MinPasswordLength {
		return fmt.Errorf("password must be %v characters long",
			pr.MinPasswordLength)
	}

	// Create user identity and save it to disk
	id, err := NewIdentity()
	if err != nil {
		return err
	}

	if !cmd.NoSave {
		cfg.SaveIdentity(id)
	}

	// Setup new user request
	nu := &v1.NewUser{
		Email:     email,
		Username:  username,
		Password:  DigestSHA3(password),
		PublicKey: hex.EncodeToString(id.Public.Key[:]),
	}

	// Print request details
	err = Print(nu, cfg.Verbose, cfg.RawJSON)
	if err != nil {
		return err
	}

	// Send request
	nur, err := c.NewUser(nu)
	if err != nil {
		return fmt.Errorf("NewUser: %v", err)
	}

	// Verify user's email address
	if cmd.Verify {
		sig := id.SignMessage([]byte(nur.VerificationToken))
		vnur, err := c.VerifyNewUser(&v1.VerifyNewUser{
			Email:             email,
			VerificationToken: nur.VerificationToken,
			Signature:         hex.EncodeToString(sig[:]),
		})
		if err != nil {
			return fmt.Errorf("VerifyNewUser: %v", err)
		}

		err = Print(vnur, cfg.Verbose, cfg.RawJSON)
		if err != nil {
			return err
		}
	}

	// Setup login request
	l := &v1.Login{
		Email:    email,
		Password: DigestSHA3(password),
	}

	// Send login request
	_, err = c.Login(l)
	if err != nil {
		return err
	}

	// Print response details
	err = Print(nur, cfg.Verbose, cfg.RawJSON)
	if err != nil {
		return err
	}

	// Pays paywall fee using faucet
	if cmd.Paywall {
		me, err := c.Me()
		if err != nil {
			return err
		}
		faucet := FaucetCmd{}
		faucet.Args.Address = me.PaywallAddress
		faucet.Args.Amount = me.PaywallAmount
		err = faucet.Execute(nil)
		if err != nil {
			return err
		}
	}

	return nil
}
