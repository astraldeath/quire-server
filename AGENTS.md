# Development conventions

- Use incremental Conventional Commits. Keep changes independently reviewable and run relevant Go tests before committing.
- Never commit runtime data, books, OAuth credentials, encryption keys, or generated binaries.
- Production images pin an exact reviewed reader revision; update its README reference together with Dockerfile.
