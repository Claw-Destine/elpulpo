# El Pulpo

El pulpo (the octopus) is a proxy service for self-hosted llm inference. It has 3 main goals:

- register usage and provide web inteface to monitor and analyse token usage and savings
- keep the configuration of multiple llm servers in one place

## Specification

Specs are kept in: `/specs` directory.

- `/specs/functional.specs.md` - holds the functional specification
- `/specs/implementation.specs.md` holds the implementation specification

## Development Guidelines

- **Do not push any changes to the remote repo.** The user will push after review.
- Follow quality rules above — minimal, elegant, no slop.
- Keep the specs up to date after every change
- Make sure README.md contains usage instructions
- Build e2e tests
