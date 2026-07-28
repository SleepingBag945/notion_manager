// SVG icon set used by the bulk-register UI. All icons match the existing
// IconRefresh/IconClock style: 24×24 viewBox, stroke 1.5, currentColor.

interface IconProps { size?: number; className?: string }

const baseProps = (size = 14) => ({
  width: size,
  height: size,
  viewBox: '0 0 24 24',
  fill: 'none',
  stroke: 'currentColor',
  strokeWidth: 1.5,
  strokeLinecap: 'round' as const,
  strokeLinejoin: 'round' as const,
})

export const IconUserPlus = ({ size, className }: IconProps) => (
  <svg {...baseProps(size)} className={className}>
    <path d="M16 21v-2a4 4 0 0 0-4-4H5a4 4 0 0 0-4 4v2" />
    <circle cx="8.5" cy="7" r="4" />
    <line x1="20" y1="8" x2="20" y2="14" />
    <line x1="23" y1="11" x2="17" y2="11" />
  </svg>
)

export const IconHistory = ({ size, className }: IconProps) => (
  <svg {...baseProps(size)} className={className}>
    <path d="M3 3v5h5" />
    <path d="M3.05 13A9 9 0 1 0 6 5.3L3 8" />
    <path d="M12 7v5l4 2" />
  </svg>
)

export const IconAlert = ({ size, className }: IconProps) => (
  <svg {...baseProps(size)} className={className}>
    <path d="M10.29 3.86 1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0z" />
    <line x1="12" y1="9" x2="12" y2="13" />
    <line x1="12" y1="17" x2="12.01" y2="17" />
  </svg>
)

export const IconActivity = ({ size, className }: IconProps) => (
  <svg {...baseProps(size)} className={className}>
    <polyline points="22 12 18 12 15 21 9 3 6 12 2 12" />
  </svg>
)

export const IconCopy = ({ size, className }: IconProps) => (
  <svg {...baseProps(size)} className={className}>
    <rect x="9" y="9" width="13" height="13" rx="2" ry="2" />
    <path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1" />
  </svg>
)

export const IconTrash = ({ size, className }: IconProps) => (
  <svg {...baseProps(size)} className={className}>
    <polyline points="3 6 5 6 21 6" />
    <path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2" />
    <line x1="10" y1="11" x2="10" y2="17" />
    <line x1="14" y1="11" x2="14" y2="17" />
  </svg>
)

export const IconRotate = ({ size, className }: IconProps) => (
  <svg {...baseProps(size)} className={className}>
    <polyline points="23 4 23 10 17 10" />
    <polyline points="1 20 1 14 7 14" />
    <path d="M3.51 9a9 9 0 0 1 14.85-3.36L23 10M1 14l4.64 4.36A9 9 0 0 0 20.49 15" />
  </svg>
)

export const IconClose = ({ size, className }: IconProps) => (
  <svg {...baseProps(size)} className={className}>
    <line x1="18" y1="6" x2="6" y2="18" />
    <line x1="6" y1="6" x2="18" y2="18" />
  </svg>
)

export const IconCheck = ({ size, className }: IconProps) => (
  <svg {...baseProps(size)} className={className}>
    <polyline points="20 6 9 17 4 12" />
  </svg>
)

export const IconX = ({ size, className }: IconProps) => (
  <svg {...baseProps(size)} className={className}>
    <circle cx="12" cy="12" r="10" />
    <line x1="15" y1="9" x2="9" y2="15" />
    <line x1="9" y1="9" x2="15" y2="15" />
  </svg>
)

export const IconMore = ({ size, className }: IconProps) => (
  <svg {...baseProps(size)} className={className}>
    <circle cx="12" cy="12" r="1" />
    <circle cx="12" cy="5" r="1" />
    <circle cx="12" cy="19" r="1" />
  </svg>
)

export const IconPlay = ({ size, className }: IconProps) => (
  <svg {...baseProps(size)} className={className}>
    <polygon points="5 3 19 12 5 21 5 3" />
  </svg>
)

export const IconExternalLink = ({ size, className }: IconProps) => (
  <svg {...baseProps(size)} className={className}>
    <path d="M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6" />
    <polyline points="15 3 21 3 21 9" />
    <line x1="10" y1="14" x2="21" y2="3" />
  </svg>
)

export const IconChevronRight = ({ size, className }: IconProps) => (
  <svg {...baseProps(size)} className={className}>
    <polyline points="9 18 15 12 9 6" />
  </svg>
)

// Power icon for enable/disable toggle.
export const IconPower = ({ size, className }: IconProps) => (
  <svg {...baseProps(size)} className={className}>
    <path d="M18.36 6.64a9 9 0 1 1-12.73 0" />
    <line x1="12" y1="2" x2="12" y2="12" />
  </svg>
)

// Spinner: stroke uses currentColor; consumer adds animate-spin.
export const IconSpinner = ({ size = 14, className }: { size?: number; className?: string }) => (
  <svg
    width={size}
    height={size}
    viewBox="0 0 24 24"
    fill="none"
    className={className}
    aria-hidden
  >
    <circle cx="12" cy="12" r="9" stroke="currentColor" strokeOpacity="0.18" strokeWidth="2.5" />
    <path d="M21 12a9 9 0 0 0-9-9" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" />
  </svg>
)
