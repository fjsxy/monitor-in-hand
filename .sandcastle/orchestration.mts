export interface IssueRecord {
  number: number;
  title: string;
  body: string;
  state: "OPEN" | "CLOSED";
  labels: Array<{ name: string }>;
  assignees: Array<{ login: string }>;
}

const REQUIRED_SECTIONS = [
  "## What to build",
  "## Acceptance criteria",
  "## Blocked by",
] as const;

export function parseBlockerNumbers(body: string): number[] {
  const heading = /^## Blocked by[ \t]*$/m.exec(body);
  if (!heading) return [];

  const sectionStart = heading.index + heading[0].length;
  const remainder = body.slice(sectionStart).replace(/^\r?\n/, "");
  const nextHeading = /^##\s/m.exec(remainder);
  const section =
    nextHeading === null ? remainder : remainder.slice(0, nextHeading.index);

  return [
    ...new Set(
      [...section.matchAll(/#(\d+)/g)].map((match) => Number(match[1])),
    ),
  ];
}

export function isAgentTicket(issue: IssueRecord): boolean {
  return (
    issue.state === "OPEN" &&
    issue.labels.some((label) => label.name === "ready-for-agent") &&
    REQUIRED_SECTIONS.every((section) => issue.body.includes(section))
  );
}

export function selectFrontier(
  issues: IssueRecord[],
  currentUser: string,
  limit: number,
): IssueRecord[] {
  const stateByNumber = new Map(
    issues.map((issue) => [issue.number, issue.state] as const),
  );

  return issues
    .filter(isAgentTicket)
    .filter(
      (issue) =>
        issue.assignees.length === 0 ||
        issue.assignees.every((assignee) => assignee.login === currentUser),
    )
    .filter((issue) =>
      parseBlockerNumbers(issue.body).every(
        (blocker) => stateByNumber.get(blocker) === "CLOSED",
      ),
    )
    .sort((left, right) => left.number - right.number)
    .slice(0, limit);
}

export function branchForIssue(issueNumber: number): string {
  return `codex/issue-${issueNumber}`;
}

export function promptArgsForIssue(
  issue: Pick<IssueRecord, "number" | "title">,
  branch: string,
): Record<string, string> {
  return {
    TASK_ID: String(issue.number),
    ISSUE_TITLE: issue.title,
    BRANCH: branch,
  };
}

export function ciRepairPromptArgs(
  issue: Pick<IssueRecord, "number" | "title">,
  branch: string,
  pullRequestNumber: number,
  failedHeadOid: string,
  failedChecks: string[],
  repairAttempt: number,
): Record<string, string> {
  return {
    ...promptArgsForIssue(issue, branch),
    PR_NUMBER: String(pullRequestNumber),
    FAILED_HEAD_OID: failedHeadOid,
    FAILED_CHECKS: failedChecks.join(", "),
    REPAIR_ATTEMPT: String(repairAttempt),
  };
}
