# Project Instructions

## Web Browsing
- **Always** use the `/browse` skill from gstack for any web browsing tasks.
- **Never** use `mcp__claude-in-chrome__*` tools.

## Available gstack Skills

### Planning & Strategy
- `/office-hours` — YC-style idea validation and design brainstorming
- `/plan-ceo-review` — Strategic/scope review of plans
- `/plan-eng-review` — Engineering architecture review of plans
- `/plan-design-review` — Design critique of plans (pre-implementation)
- `/plan-devex-review` — Developer experience review of plans
- `/autoplan` — Run all plan reviews automatically

### Design
- `/design-consultation` — Build a design system from scratch (DESIGN.md)
- `/design-shotgun` — Generate multiple AI design variants to compare
- `/design-html` — Turn approved mockups into production HTML/CSS
- `/design-review` — Visual QA on a live site, iteratively fix issues

### Shipping & Deployment
- `/ship` — Full ship workflow (test, review, version, commit, push, PR)
- `/land-and-deploy` — Merge PR, wait for CI/deploy, verify production
- `/canary` — Post-deploy monitoring for regressions
- `/setup-deploy` — Configure deployment settings

### Quality & Review
- `/review` — Pre-landing PR review
- `/qa` — Test web app and fix bugs found
- `/qa-only` — Test web app and report bugs (no fixes)
- `/devex-review` — Live developer experience audit
- `/benchmark` — Performance regression detection
- `/codex` — Second opinion via OpenAI Codex (review/challenge/consult)
- `/cso` — Security audit (OWASP, threat modeling, supply chain)

### Development Workflow
- `/browse` — Headless browser for QA and dogfooding
- `/connect-chrome` — Connect to a real Chrome browser
- `/setup-browser-cookies` — Import cookies for authenticated testing
- `/investigate` — Systematic root-cause debugging
- `/retro` — Weekly engineering retrospective
- `/document-release` — Sync docs after shipping
- `/learn` — Manage project learnings across sessions
- `/setup-gbrain` — Set up gbrain for this agent

### Safety
- `/careful` — Warn before destructive commands
- `/freeze` — Restrict edits to one directory
- `/guard` — Combine `/careful` + `/freeze`
- `/unfreeze` — Lift the freeze boundary

### Maintenance
- `/gstack-upgrade` — Upgrade gstack to the latest version

---

**After setup:** Ask the user whether to add gstack to this project so teammates also get these skills.
