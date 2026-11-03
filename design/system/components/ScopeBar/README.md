# ScopeBar

Shows and switches where every action on the page applies: organization, project, environment.

**You provide** `org`, `project`, `environment`, `protected` when the environment requires approval, and `onSelect` to open the switcher for a level.

- It lives at the left of the top bar on every page. Actions, the command bar and plans all run in this scope.
- A protected environment always shows its lock here, so nobody deploys to production by accident.
- Never hide a level to save space; truncate the project name instead.
