export const DEFAULT_AGENT_IDLE_TIMEOUT_SECONDS = 30 * 60;
export const DEFAULT_MAX_CI_REPAIR_ATTEMPTS = 2;

type Environment = Record<string, string | undefined>;

function positiveInteger(value: string | undefined, fallback: number): number {
  const parsed = Number(value);
  return Number.isInteger(parsed) && parsed > 0 ? parsed : fallback;
}

function nonNegativeInteger(
  value: string | undefined,
  fallback: number,
): number {
  const parsed = Number(value);
  return Number.isInteger(parsed) && parsed >= 0 ? parsed : fallback;
}

export function resolveAgentIdleTimeoutSeconds(
  processEnvironment: Environment,
  localEnvironment: Environment,
): number {
  return positiveInteger(
    processEnvironment.SANDCASTLE_IDLE_TIMEOUT_SECONDS ||
      localEnvironment.SANDCASTLE_IDLE_TIMEOUT_SECONDS,
    DEFAULT_AGENT_IDLE_TIMEOUT_SECONDS,
  );
}

export function resolveMaxCIRepairAttempts(
  processEnvironment: Environment,
  localEnvironment: Environment,
): number {
  return nonNegativeInteger(
    processEnvironment.SANDCASTLE_MAX_CI_REPAIR_ATTEMPTS ||
      localEnvironment.SANDCASTLE_MAX_CI_REPAIR_ATTEMPTS,
    DEFAULT_MAX_CI_REPAIR_ATTEMPTS,
  );
}

export function withAgentIdleTimeout<T extends object>(
  idleTimeoutSeconds: number,
  options: T,
): T & { idleTimeoutSeconds: number } {
  return { ...options, idleTimeoutSeconds };
}
