import { expect, test } from '@playwright/test';

const inviteServerURL = 'http://127.0.0.1:2446';
const signupTarget = 'https://signup.example.test/create?source=invite';
const validSecret = 'a'.repeat(64);

test('valid invite fragment is stripped and attached to the configured signup target', async ({ page }) => {
  await page.goto(`${inviteServerURL}/invite#${validSecret}`);

  await expect(page).toHaveURL(`${inviteServerURL}/invite`);
  expect(await page.evaluate(() => window.location.hash)).toBe('');
  await expect(page.getByRole('heading', { name: 'You’re invited to Teagram', level: 1 })).toBeVisible();
  await expect(page.getByRole('link', { name: 'Create account on Teagram Web' })).toBeVisible();
  await expect(page.getByRole('link', { name: 'Create account on Teagram Web' })).toHaveAttribute(
    'href',
    `${signupTarget}#${validSecret}`,
  );
});

test('malformed invite fragment is stripped and keeps signup controls hidden', async ({ page }) => {
  await page.goto(`${inviteServerURL}/invite#${'A'.repeat(64)}`);

  await expect(page).toHaveURL(`${inviteServerURL}/invite`);
  expect(await page.evaluate(() => window.location.hash)).toBe('');
  await expect(page.getByRole('heading', { name: 'This invite can’t be used', level: 1 })).toBeVisible();
  await expect(page.locator('#invite-actions')).toBeHidden();
  await expect(page.getByRole('link', { name: 'Create account on Teagram Web' })).toBeHidden();
});
