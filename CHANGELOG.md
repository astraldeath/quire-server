# Server changes

This file describes user-visible changes included in the current main container build. Main builds are rolling previews, not numbered stable releases. Keep this entry concise and update it before publishing a change; move older entries under dated headings when starting the next change summary.

## Current main build

- Signed-in readers can see the installed server version, published changes, and whether a newer server build is available.
- Administrators receive instructions for updating their Docker deployment. Updates remain an explicit operator action.
- Update checks retain the last successful result when GitHub is unavailable and identify local development builds as unknown.

## Entry template

When preparing a change, replace the current entry with a short description of what changed for readers or operators. Include migration steps or compatibility constraints when required. Do not use raw commit lists as release notes.
