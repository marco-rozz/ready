// Optional checks run after saving and never undo a published configuration.
package initwizard

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/internal/llm"
	"github.com/marco-rozz/ready/internal/orchestrator"
	"github.com/rs/zerolog"
)

const probeBudget = 15 * time.Second

func (o Options) runVerification(ctx context.Context, in promptReader, res config.Resolution, choice ProviderChoice, candidate *config.Config) {
	out := o.stdout()
	_, _ = fmt.Fprintln(out, "\nOptional checks — your configuration is already saved.\nYou may skip these checks; a skipped or failed check does not delete your files.")
	ref := choice.Instance + "::" + choice.ModelID
	if choice.Provider == config.ProviderOpenAICodex {
		o.offerAndRunCodexLogin(ctx, in, res, choice)

		return
	}
	if !askProceed(ctx, in, out, "Check the connection and whether "+ref+" is listed by the provider (does not run the model)? [y/N]: ") {
		printVerificationSkip(ctx, out, "Connection/model check skipped")

		return
	}
	if err := llm.PreflightCatalog(ctx, &candidate.LLM, ref, zerolog.Nop()); err != nil {
		_, _ = fmt.Fprintf(out, "Connection/model check failed:\n  %v\nYour files are saved, but READY may be unable to answer until this is resolved.\nCheck your server address, model ID and API key in %s.\nFor a local provider, also check that the server is running.\nTo change the provider or key, run:\n  %s\n", err, res.Path, o.initCommand(res.AgentDir))

		return
	}
	_, _ = fmt.Fprintln(out, "Provider check completed. This check alone does not prove the model can answer; the optional action test below checks that.")
	if !askProceed(ctx, in, out, "Ask the model to perform a small test action (up to 15 seconds, without your conversation history)? [y/N]: ") {
		printVerificationSkip(ctx, out, "Model action test skipped")

		return
	}
	model, err := llm.BuildForReference(ctx, &candidate.LLM, ref)
	if err != nil {
		_, _ = fmt.Fprintf(out, "The model action test could not start: %v\nYour configuration is still saved. Review the provider settings in %s before chatting.\n", err, res.Path)

		return
	}
	if err := orchestrator.RunSyntheticProbe(ctx, model, probeBudget); err != nil {
		_, _ = fmt.Fprintf(out, "The model action test did not succeed: %v\nYour configuration is still saved. A local model may take longer to load;\ncheck the provider settings if READY cannot answer after starting.\n", err)

		return
	}
	_, _ = fmt.Fprintln(out, "Model action test passed: the model completed the test action.")
}

func (o Options) offerAndRunCodexLogin(ctx context.Context, in promptReader, res config.Resolution, choice ProviderChoice) {
	out := o.stdout()
	login := o.loginCommand(res.AgentDir, choice.Instance)
	_, _ = fmt.Fprintf(out, `
%s needs ChatGPT sign-in before it can answer. Your sign-in is kept
inside this agent directory. No API key is required.
To sign in later, use this command for the same agent:

  %s

`, choice.Provider, login)
	if !askProceed(ctx, in, out, "Sign in to ChatGPT now? [y/N]: ") {
		printVerificationSkip(ctx, out, "ChatGPT sign-in skipped; use the login command above before chatting")

		return
	}
	_, _ = fmt.Fprintln(out, "Use a browser on this computer, or a device code you can open on another device?\nSign-in method (B = browser, D = device code) [B]: ")
	answer, err := in.line(ctx)
	if err != nil {
		_, _ = fmt.Fprintln(out, "Sign-in was not started. Your configuration is saved; run the login command above before chatting.")

		return
	}
	device := strings.EqualFold(strings.TrimSpace(answer), "d")
	if o.RunCodexLogin == nil {
		_, _ = fmt.Fprintln(out, "Sign-in is unavailable inside this wizard. Run the login command above before chatting.")

		return
	}
	if err := o.RunCodexLogin(ctx, res.AgentDir, device); err != nil {
		_, _ = fmt.Fprintf(out, "ChatGPT sign-in failed: %v\nYour configuration is still saved. Retry sign-in before chatting:\n  %s\n", err, login)

		return
	}
	_, _ = fmt.Fprintln(out, "ChatGPT sign-in completed. READY will check model availability when it starts.")
}

func printVerificationSkip(_ context.Context, out io.Writer, why string) {
	_, _ = fmt.Fprintf(out, "%s. Your configuration is saved; the connection/model has not been fully tested by this wizard.\n", why)
}

func askProceed(ctx context.Context, in promptReader, out io.Writer, question string) bool {
	_, _ = fmt.Fprint(out, question)
	answer, err := in.line(ctx)
	if err != nil {
		return false
	}
	answer = strings.TrimSpace(strings.ToLower(answer))

	return answer == "y" || answer == "yes"
}
