import { test, expect } from '@playwright/test';
import { mockWorkspace, member } from './workspace-fixture.mjs';

for (const [width, height] of [[320,568], [390,844], [1280,800], [1440,900]]) {
  test.describe(`workspace ${width}px`, () => {
    test.use({ viewport: { width, height }, hasTouch: width < 600 });

    test('work summary, stage and actions fit the panel', async ({ page }) => {
      await mockWorkspace(page);
      await page.goto('/#work');
      const row = page.locator('#workTable tr[data-root]').nth(1);
      await expect(row.locator('strong')).toContainText('发布清单');
      await expect(row.getByText('待验收')).toBeVisible();
      for (const button of await row.getByRole('button').all()) {
        const bounds = await button.boundingBox();
        expect(bounds.x + bounds.width).toBeLessThanOrEqual(width);
        if (width < 600) expect(bounds.height).toBeGreaterThanOrEqual(44);
      }
      expect(await page.locator('.table-wrap').first().evaluate(el => el.scrollWidth - el.clientWidth)).toBeLessThanOrEqual(1);
      expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(width);
    });

    test('Muse status, composer and keyboard settings fit the viewport', async ({ page }) => {
      const waiting = [390, 1440].includes(width);
      await mockWorkspace(page, waiting ? 'waiting' : 'timeout');
      await page.goto(`/#muse-chat?peer=${member}`);
      await expect(page.locator('#mchatStream')).toContainText(waiting ? '已投递，等待回复' : '已投递，等待回复超时');
      await expect(page.locator('#mchatStop')).toBeVisible({ visible: waiting });
      const send = await page.locator('#mchatSend').boundingBox();
      expect(send.y + send.height).toBeLessThanOrEqual(height + 1);
      expect(send.x + send.width).toBeLessThanOrEqual(width);
      await expect(page.locator('#mchatVisibility')).toContainText('成员本人可见');
      expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(width);
      const settings = page.getByRole('button', { name: '对话设置' });
      await settings.focus();
      await page.keyboard.press('Enter');
      await expect(page.getByRole('dialog')).toBeVisible();
      await page.getByLabel('等待回复的时长').selectOption('60');
      await page.keyboard.press('Escape');
      await expect(settings).toBeFocused();
      await expect(page.getByRole('dialog')).toHaveCount(0);
    });
  });
}
