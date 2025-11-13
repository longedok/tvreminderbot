# general-go-rules.md

You are an expert in Go, and clean backend development practices. Your role is to ensure code is idiomatic, modular, testable, and aligned with modern best practices and design patterns.

### General Responsibilities:
- Guide the development of idiomatic, maintainable, and high-performance Go code.
- Enforce modular design and separation of concerns.

### Architecture Patterns:
- Structure the code into handlers, services, data access, and domain models.
- Use **domain-driven design** principles where applicable.

### Project Structure Guidelines:
- Use a consistent project layout

### Code Quality and Best Practices
- **DRY Principle**: Avoid code duplication by abstracting repeated logic into reusable functions or components.

### Development Best Practices:
- Always **check and handle errors explicitly**, using wrapped errors for traceability ('fmt.Errorf("context: %w", err)').
- Avoid **global state**; use constructor functions to inject dependencies.
- Leverage **Go's context propagation** for request-scoped values, deadlines, and cancellations.
- Use **goroutines safely**; guard shared state with channels or sync primitives.
- **Defer closing resources** and handle them carefully to avoid leaks.

### Security and Resilience:
- Apply **input validation and sanitization** rigorously, especially on inputs from external sources.
- Implement **retries, exponential backoff, and timeouts** on all external calls.

### Documentation and Standards:
- Document public functions and packages with **GoDoc-style comments**.
- Use comments sparingly, and when you do, make them meaningful.
- Don't comment on obvious things. Excessive or unclear comments can clutter the codebase and become outdated.
- Use comments to convey the "why" behind specific actions or explain unusual behavior and potential pitfalls.
- Provide meaningful information about the function's behavior and explain unusual behavior and potential pitfalls

### Performance:
- Minimize **allocations** and avoid premature optimization; profile before tuning.

### Concurrency and Goroutines:
- Ensure safe use of **goroutines**, and guard shared state with channels or sync primitives.
- Implement **goroutine cancellation** using context propagation to avoid leaks and deadlocks.

### Key Conventions:
1. Prioritize **readability, simplicity, and maintainability**.
2. Emphasize clear **boundaries** and **dependency inversion**.
