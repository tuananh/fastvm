import { CommandFailedError } from "./errors.js";

/** One completed guest command. */
export class ExecResult {
  readonly exitCode: number;
  readonly stdout: string;
  readonly stderr: string;

  constructor(input: { exitCode: number; stdout?: string; stderr?: string }) {
    this.exitCode = input.exitCode;
    this.stdout = input.stdout ?? "";
    this.stderr = input.stderr ?? "";
  }

  get ok(): boolean {
    return this.exitCode === 0;
  }

  assertSuccess(): this {
    if (!this.ok) {
      throw new CommandFailedError({
        exitCode: this.exitCode,
        stdout: this.stdout,
        stderr: this.stderr,
      });
    }
    return this;
  }
}
