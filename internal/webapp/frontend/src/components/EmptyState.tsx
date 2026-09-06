import { Button } from "@/components/ui/button";
import { GuideCode } from "./ConnectGuide";
import { agentPrompt } from "../lib/prompt";

// Onboarding: a signed-in account with no projects shouldn't hit a blank
// sidebar. Two paths in, in the order most people want them — create the
// project here and pick what it starts from, or paste the install prompt into
// a coding agent and let it do the whole thing. The by-hand route stays a
// docs link away.
//
// This is the first thing a zero-project account sees — nothing opens over
// it. Both cards are entry points: the button creates the project here, the
// paste-prompt hands the whole job to an agent.

export function EmptyState({
  onNew,
  canCreate,
}: {
  onNew: () => void;
  canCreate: boolean;
}) {
  return (
    <div className="onboard">
      <h1>Welcome to BearDrive</h1>
      <p>You're signed in, but you're not part of any project yet.</p>
      {canCreate && (
        <div className="ob-card ob-start">
          <h3>Start a project</h3>
          <p>
            Name it and pick what it starts from — a structure, or nothing at all. Then connect a
            folder on any machine and it stays in sync.
          </p>
          <Button variant="primary" id="ob-new" onClick={onNew}>
            New project
          </Button>
        </div>
      )}
      <div className="ob-card ob-agent">
        <h3>{canCreate ? "Or let your agent do it" : "Connect a new drive to your project"}</h3>
        <p>
          Paste into your coding agent — Claude Code, Cowork, Codex, Gemini CLI, Hermes — in the
          folder where you want the files. It creates the project and starts syncing:
        </p>
        <GuideCode code={agentPrompt(window.location.origin)} />
        <p className="ob-alt">
          <a href="https://docs.beardrive.ai/manual/setup-by-hand/" target="_blank" rel="noreferrer">
            Or start a project manually →
          </a>
        </p>
      </div>
    </div>
  );
}
