export const REAL_CLIENT_SCENARIOS = Object.freeze([
  'sign-in', 'first-message', 'message', 'custom-emoji', 'group', 'channel', 'poll'
]);

export function selectRealClientScenarios({required, optional}) {
  if(!Array.isArray(required) || required.length === 0 || !Array.isArray(optional) ||
      [...required, ...optional].some((scenario) => typeof scenario !== 'string' || !scenario)) {
    throw new Error('web scenario contract must declare required and optional scenarios');
  }

  const unsupportedRequired = required.filter((scenario) => !REAL_CLIENT_SCENARIOS.includes(scenario));
  if(unsupportedRequired.length) {
    throw new Error(`server runner does not support required scenarios: ${unsupportedRequired.join(',')}`);
  }

  const supported = new Set([...required, ...optional]);
  return REAL_CLIENT_SCENARIOS.filter((scenario) => supported.has(scenario));
}
