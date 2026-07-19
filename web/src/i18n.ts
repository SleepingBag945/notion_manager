import i18n from 'i18next';
import { initReactI18next } from 'react-i18next';
import zh from './locales/zh.json';
import en from './locales/en.json';

const savedLang = localStorage.getItem('notion-manager-lang') ?? 'zh';

i18n
  .use(initReactI18next)
  .init({
    resources: {
      zh: { translation: zh },
      en: { translation: en }
    },
    lng: savedLang,
    fallbackLng: 'zh',
    interpolation: {
      escapeValue: false // react already safes from xss
    }
  });

export default i18n;
