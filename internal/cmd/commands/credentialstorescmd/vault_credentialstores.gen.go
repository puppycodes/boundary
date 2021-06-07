// Code generated by "make cli"; DO NOT EDIT.
package credentialstorescmd

import (
	"errors"
	"fmt"

	"github.com/hashicorp/boundary/api"
	"github.com/hashicorp/boundary/api/credentialstores"
	"github.com/hashicorp/boundary/internal/cmd/base"
	"github.com/hashicorp/boundary/internal/cmd/common"
	"github.com/hashicorp/boundary/sdk/strutil"
	"github.com/mitchellh/cli"
	"github.com/posener/complete"
)

func initVaultFlags() {
	flagsOnce.Do(func() {
		extraFlags := extraVaultActionsFlagsMapFunc()
		for k, v := range extraFlags {
			flagsVaultMap[k] = append(flagsVaultMap[k], v...)
		}
	})
}

var (
	_ cli.Command             = (*VaultCommand)(nil)
	_ cli.CommandAutocomplete = (*VaultCommand)(nil)
)

type VaultCommand struct {
	*base.Command

	Func string

	plural string

	extraVaultCmdVars
}

func (c *VaultCommand) AutocompleteArgs() complete.Predictor {
	initVaultFlags()
	return complete.PredictAnything
}

func (c *VaultCommand) AutocompleteFlags() complete.Flags {
	initVaultFlags()
	return c.Flags().Completions()
}

func (c *VaultCommand) Synopsis() string {
	if extra := extraVaultSynopsisFunc(c); extra != "" {
		return extra
	}

	synopsisStr := "credential store"

	synopsisStr = fmt.Sprintf("%s %s", "vault-type", synopsisStr)

	return common.SynopsisFunc(c.Func, synopsisStr)
}

func (c *VaultCommand) Help() string {
	initVaultFlags()

	var helpStr string
	helpMap := common.HelpMap("credential store")

	switch c.Func {
	default:

		helpStr = c.extraVaultHelpFunc(helpMap)
	}

	// Keep linter from complaining if we don't actually generate code using it
	_ = helpMap
	return helpStr
}

var flagsVaultMap = map[string][]string{

	"create": {"scope-id", "name", "description"},

	"update": {"id", "name", "description", "version"},
}

func (c *VaultCommand) Flags() *base.FlagSets {
	if len(flagsVaultMap[c.Func]) == 0 {
		return c.FlagSet(base.FlagSetNone)
	}

	set := c.FlagSet(base.FlagSetHTTP | base.FlagSetClient | base.FlagSetOutputFormat)
	f := set.NewFlagSet("Command Options")
	common.PopulateCommonFlags(c.Command, f, "vault-type credential store", flagsVaultMap[c.Func])

	extraVaultFlagsFunc(c, set, f)

	return set
}

func (c *VaultCommand) Run(args []string) int {
	initVaultFlags()

	switch c.Func {
	case "":
		return cli.RunResultHelp
	}

	c.plural = "vault-type credential store"
	switch c.Func {
	case "list":
		c.plural = "vault-type credential stores"
	}

	f := c.Flags()

	if err := f.Parse(args); err != nil {
		c.PrintCliError(err)
		return base.CommandUserError
	}

	if strutil.StrListContains(flagsVaultMap[c.Func], "id") && c.FlagId == "" {
		c.PrintCliError(errors.New("ID is required but not passed in via -id"))
		return base.CommandUserError
	}

	var opts []credentialstores.Option

	if strutil.StrListContains(flagsVaultMap[c.Func], "scope-id") {
		switch c.Func {
		case "create":
			if c.FlagScopeId == "" {
				c.PrintCliError(errors.New("Scope ID must be passed in via -scope-id or BOUNDARY_SCOPE_ID"))
				return base.CommandUserError
			}
		}
	}

	client, err := c.Client()
	if err != nil {
		c.PrintCliError(fmt.Errorf("Error creating API client: %s", err.Error()))
		return base.CommandCliError
	}
	credentialstoresClient := credentialstores.NewClient(client)

	switch c.FlagName {
	case "":
	case "null":
		opts = append(opts, credentialstores.DefaultName())
	default:
		opts = append(opts, credentialstores.WithName(c.FlagName))
	}

	switch c.FlagDescription {
	case "":
	case "null":
		opts = append(opts, credentialstores.DefaultDescription())
	default:
		opts = append(opts, credentialstores.WithDescription(c.FlagDescription))
	}

	switch c.FlagRecursive {
	case true:
		opts = append(opts, credentialstores.WithRecursive(true))
	}

	if c.FlagFilter != "" {
		opts = append(opts, credentialstores.WithFilter(c.FlagFilter))
	}

	var version uint32

	switch c.Func {
	case "update":
		switch c.FlagVersion {
		case 0:
			opts = append(opts, credentialstores.WithAutomaticVersioning(true))
		default:
			version = uint32(c.FlagVersion)
		}
	}

	if ok := extraVaultFlagsHandlingFunc(c, f, &opts); !ok {
		return base.CommandUserError
	}

	var result api.GenericResult

	switch c.Func {

	case "create":
		result, err = credentialstoresClient.Create(c.Context, "vault", c.FlagScopeId, opts...)

	case "update":
		result, err = credentialstoresClient.Update(c.Context, c.FlagId, version, opts...)

	}

	result, err = executeExtraVaultActions(c, result, err, credentialstoresClient, version, opts)

	if err != nil {
		if apiErr := api.AsServerError(err); apiErr != nil {
			var opts []base.Option

			c.PrintApiError(apiErr, fmt.Sprintf("Error from controller when performing %s on %s", c.Func, c.plural), opts...)
			return base.CommandApiError
		}
		c.PrintCliError(fmt.Errorf("Error trying to %s %s: %s", c.Func, c.plural, err.Error()))
		return base.CommandCliError
	}

	output, err := printCustomVaultActionOutput(c)
	if err != nil {
		c.PrintCliError(err)
		return base.CommandUserError
	}
	if output {
		return base.CommandSuccess
	}

	switch c.Func {
	}

	switch base.Format(c.UI) {
	case "table":
		c.UI.Output(printItemTable(result))

	case "json":
		if ok := c.PrintJsonItem(result); !ok {
			return base.CommandCliError
		}
	}

	return base.CommandSuccess
}

var (
	extraVaultActionsFlagsMapFunc = func() map[string][]string { return nil }
	extraVaultSynopsisFunc        = func(*VaultCommand) string { return "" }
	extraVaultFlagsFunc           = func(*VaultCommand, *base.FlagSets, *base.FlagSet) {}
	extraVaultFlagsHandlingFunc   = func(*VaultCommand, *base.FlagSets, *[]credentialstores.Option) bool { return true }
	executeExtraVaultActions      = func(_ *VaultCommand, inResult api.GenericResult, inErr error, _ *credentialstores.Client, _ uint32, _ []credentialstores.Option) (api.GenericResult, error) {
		return inResult, inErr
	}
	printCustomVaultActionOutput = func(*VaultCommand) (bool, error) { return false, nil }
)
