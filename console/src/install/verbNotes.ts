// What each verb lets somebody do, for the role editor.
//
// The verb itself stays the label — it is what the API, the CLI and the audit
// log call it, and machine text is shown verbatim — and this is the sentence
// under it. A verb with no sentence here is shown with none; the test beside
// this file keeps every verb the console knows about covered.

export const VERB_NOTES: Record<string, string> = {
  'install.view': 'See the installation: accounts, adapters, policy and capacity.',
  'install.users.manage': 'Add, change and remove accounts, groups and roles.',
  'install.policy.manage': 'Change host policy.',
  'install.adapters.manage': 'Add, change and remove adapters, and restart Pando to apply them.',
  'install.audit.read': 'Read the audit log.',
  'install.backup.manage': 'Take, verify and restore backups of the whole installation.',
  'install.apps.view': 'See every app and its settings, including apps added later.',
  'install.apps.logs.read': 'Read every app’s output and its deploy logs.',
  'install.apps.deploy': 'Deploy and roll back every app.',
  'install.apps.restart': 'Stop, start and restart every app.',
  'install.apps.spec.edit': 'Change how every app is built and run.',
  'install.apps.secrets.write': 'Set and rotate every app’s secrets.',
  'install.apps.secrets.read': 'Read every app’s secret values.',
  'install.apps.exec': 'Open a terminal inside any running app.',
  'install.apps.grants.manage': 'Share every app, and change who can reach each one.',
  'install.apps.routing.override': 'Choose any app’s address.',
  'install.apps.resources.override': 'Change any app’s CPU, memory and disk limits.',
  'install.apps.egress.tighten': 'Narrow where any app can connect out to, within the installation’s rules.',
  'install.apps.egress.loosen':
    'Loosen the installation’s egress rules for any app, where policy allows it. Covers narrowing them too.',
  'install.apps.delete': 'Delete any app.',
  'install.tokens.manage': 'Create, list and revoke service tokens.',
  'install.deploys.approve': 'Approve or reject any deploy that needs approval, on any app, including their own.',
  'install.upgrade': 'Upgrade Pando itself to a newer release, from the Updates screen.',
  'install.events.manage':
    'Subscribe to events across the whole installation, sign-ins included, and see or change anyone’s subscriptions.',
  'install.audit.export':
    'Send the audit log off the installation: add, change or remove an audit sink such as a SIEM.',
  'app.create': 'Add new apps.',

  'app.view': 'See the app and its settings.',
  'app.logs.read': 'Read the app’s output and its deploy logs.',
  'app.deploy': 'Deploy the app and roll it back.',
  'app.restart': 'Stop, start and restart the app.',
  'app.spec.edit': 'Change how the app is built and run.',
  'app.secrets.write': 'Set and rotate the app’s secrets.',
  'app.secrets.read': 'Read the app’s secret values.',
  'app.exec': 'Open a terminal inside the running app.',
  'app.grants.manage': 'Share the app, and change who can reach it.',
  'app.routing.override': 'Choose the app’s address.',
  'app.resources.override': 'Change the app’s CPU, memory and disk limits.',
  'app.egress.tighten': 'Narrow where the app can connect out to, within the installation’s rules.',
  'app.egress.loosen':
    'Loosen the installation’s egress rules for the app, where policy allows it. Covers narrowing them too.',
  'app.deploy.approve': 'Approve or reject this app’s deploys that need approval, including their own.',
  'app.delete': 'Delete the app.',
};
