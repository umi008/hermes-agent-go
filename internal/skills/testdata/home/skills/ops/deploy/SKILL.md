---
name: deploy
required_environment_variables:
  - name: DEPLOY_TOKEN
    prompt: Deploy token
    help: Create one in the dashboard.
---
# Deploy

Ship the build with ${HERMES_SKILL_DIR}/run.sh in session ${HERMES_SESSION_ID}.
