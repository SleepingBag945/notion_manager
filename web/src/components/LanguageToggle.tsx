import { useTranslation } from 'react-i18next';

export function LanguageToggle() {
  const { i18n } = useTranslation();
  const isZh = i18n.language === 'zh';

  const toggle = () => {
    const next = isZh ? 'en' : 'zh';
    i18n.changeLanguage(next);
    localStorage.setItem('notion-manager-lang', next);
  };

  return (
    <button
      onClick={toggle}
      title={isZh ? 'Switch to English' : '切换至中文'}
      className="flex items-center gap-1.5 px-2.5 py-1 text-[12px] font-medium text-text-secondary hover:text-text-primary bg-bg-card hover:bg-bg-input border border-border rounded-md cursor-pointer transition-colors outline-none"
    >
      <span className="text-[14px]">🌐</span>
      <span>{isZh ? 'EN' : '中文'}</span>
    </button>
  );
}
