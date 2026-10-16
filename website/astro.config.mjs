// @ts-check
import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';

const repo = 'https://github.com/keakon/chord-gateway';

export default defineConfig({
  site: 'https://keakon.github.io',
  base: '/chord-gateway',
  trailingSlash: 'always',
  integrations: [
    starlight({
      title: 'chord-gateway',
      description: 'Connect WeChat, Feishu, Telegram, WeCom, DingTalk, Discord, and Slack chats to local chord headless processes.',
      social: [
        { icon: 'github', label: 'GitHub', href: repo },
      ],
      defaultLocale: 'root',
      locales: {
        root: { label: 'English', lang: 'en' },
        zh: { label: '中文', lang: 'zh-CN' },
      },
      lastUpdated: true,
      pagination: true,
      sidebar: [
        {
          label: 'Getting started',
          translations: { 'zh-CN': '入门' },
          items: [
            { slug: 'quickstart', translations: { 'zh-CN': '快速开始' } },
            { slug: 'cookbook', translations: { 'zh-CN': 'Cookbook' } },
            { slug: 'im', translations: { 'zh-CN': 'IM 接入总览' } },
          ],
        },
        {
          label: 'IM platforms',
          translations: { 'zh-CN': 'IM 平台' },
          items: [
            { slug: 'wechat', translations: { 'zh-CN': '微信 iLink' } },
            { slug: 'feishu', translations: { 'zh-CN': '飞书' } },
            { slug: 'telegram', translations: { 'zh-CN': 'Telegram' } },
            { slug: 'wecom', translations: { 'zh-CN': '企业微信' } },
            { slug: 'dingtalk', translations: { 'zh-CN': '钉钉' } },
            { slug: 'discord', translations: { 'zh-CN': 'Discord' } },
            { slug: 'slack', translations: { 'zh-CN': 'Slack' } },
          ],
        },
        {
          label: 'Daily usage',
          translations: { 'zh-CN': '日常使用' },
          items: [
            { slug: 'usage', translations: { 'zh-CN': '使用指南' } },
            { slug: 'event-visibility', translations: { 'zh-CN': '事件可见性' } },
          ],
        },
        {
          label: 'Reference',
          translations: { 'zh-CN': '参考' },
          items: [
            { slug: 'configuration', translations: { 'zh-CN': '配置参考' } },
            { slug: 'operations', translations: { 'zh-CN': '运维说明' } },
            { slug: 'compatibility', translations: { 'zh-CN': '兼容策略' } },
            { slug: 'adapter-development', translations: { 'zh-CN': 'Adapter 开发' } },
            { slug: 'changelog', translations: { 'zh-CN': '更新日志' } },
          ],
        },
        {
          label: 'Safety',
          translations: { 'zh-CN': '安全' },
          items: [
            { slug: 'permissions-and-safety', translations: { 'zh-CN': '权限与安全边界' } },
          ],
        },
        {
          label: 'Troubleshooting',
          translations: { 'zh-CN': '排障' },
          items: [
            { slug: 'troubleshooting', translations: { 'zh-CN': '故障排查' } },
          ],
        },
      ],
      customCss: ['./src/styles/custom.css'],
    }),
  ],
});
