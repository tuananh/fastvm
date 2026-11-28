export class FastVMError extends Error {
  constructor(message: string, options?: { cause?: unknown }) {
    super(message, options);
    this.name = new.target.name;
  }
}

export class ConnectionError extends FastVMError {}

export class ApiError extends FastVMError {
  readonly status: number;
  readonly kind: string | undefined;
  readonly body: unknown;

  constructor(status: number, message: string, kind?: string, body?: unknown) {
    super(message);
    this.status = status;
    this.kind = kind;
    this.body = body;
  }
}

export class NotFoundError extends ApiError {}
export class ConflictError extends ApiError {}
export class InvalidError extends ApiError {}

export class CommandFailedError extends FastVMError {
  readonly result: {
    exitCode: number;
    stdout: string;
    stderr: string;
  };

  constructor(result: { exitCode: number; stdout: string; stderr: string }) {
    super(`command exited with code ${result.exitCode}`);
    this.result = result;
  }
}

export class TimeoutError extends FastVMError {}
