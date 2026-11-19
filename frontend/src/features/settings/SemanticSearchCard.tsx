import { useEffect, useState } from 'react';
import { Events } from '@wailsio/runtime';
import { App } from '../../../bindings/zopresent';
import type {
  SemanticAttributionView,
  SemanticDomainStatusView,
  SemanticStatusView,
} from '../../../bindings/zopresent/models';
import { Icon } from '../../components/Icon';
import { formatBytes } from '../../lib/format';

// The progress payload travels only as an event, so it has no generated model;
// its shape is mirrored here. Domain is set for index progress ("bible" or
// "song") so each index renders its own bar.
type SemanticProgressView = {
  phase: 'download' | 'index';
  domain?: string;
  file: string;
  done: number;
  total: number;
  bytes: number;
  totalBytes: number;
};

type DomainKey = 'bible' | 'song';

const DOMAINS: { key: DomainKey; label: string; hint: string }[] = [
  {
    key: 'bible',
    label: 'Scripture',
    hint: 'Indexes every verse of the installed translations.',
  },
  {
    key: 'song',
    label: 'Songs',
    hint: 'Indexes song titles and lyric sections.',
  },
];

function errorMessage(error: unknown): string {
  // A failed call can reject without any text at all (the runtime's browser
  // fallback does); an empty message would render a blank alert with no
  // explanation, so fall back to words the operator can act on.
  const message = error instanceof Error ? error.message : String(error);
  return message.trim() || 'The app backend did not answer.';
}

const PRIMARY_BUTTON =
  'min-h-7 inline-flex flex-1 items-center justify-center gap-1.5 px-2 py-1 border border-blue! bg-blue! ' +
  'text-white! rounded-md text-[10.5px] font-medium whitespace-nowrap ' +
  'not-disabled:hover:bg-[#5B88FF]! not-disabled:hover:border-[#5B88FF]! ' +
  'disabled:text-secondary! disabled:bg-[#142033]! disabled:border-[#2A3950]!';

const QUIET_BUTTON =
  'min-h-7 inline-flex items-center justify-center gap-1.5 px-2 py-1 border border-[#35445C] ' +
  'rounded-md bg-elevated text-[10.5px] font-medium whitespace-nowrap ' +
  'disabled:text-secondary disabled:bg-[#142033] disabled:border-[#2A3950]';

function badge(state: string | undefined) {
  switch (state) {
    case 'ready':
      return { text: 'Ready', cls: 'border-[#1E4D40]! bg-[#0E2A24]! text-[#6EE7C0]!' };
    case 'stale':
      return { text: 'Needs rebuild', cls: 'border-[#4D3F1E]! bg-[#2B2413]! text-amber!' };
    case 'building':
      return { text: 'Building', cls: 'border-[#2A3950]! bg-elevated! text-blue!' };
    default:
      return { text: 'Not built', cls: 'border-border! bg-app! text-secondary!' };
  }
}

function domainCounts(domain: SemanticDomainStatusView): string {
  if (domain.state === 'ready') {
    return `${domain.indexedDocs.toLocaleString()} documents indexed.`;
  }
  if (domain.state === 'stale') {
    return `${domain.indexedDocs.toLocaleString()} indexed · ${domain.pendingDocs.toLocaleString()} new or changed${
      domain.staleDocs > 0 ? ` · ${domain.staleDocs.toLocaleString()} removed` : ''
    } since the last build.`;
  }
  return `${domain.totalDocs.toLocaleString()} documents to index. The first build takes a while; later builds only touch what changed.`;
}

// SemanticSearchCard installs the meaning-search model and builds the
// scripture and song indexes. Scripture and songs are indexed separately, so
// each has its own build button and progress; everything runs on the Go side
// and reports through events, and this only renders it.
export function SemanticSearchCard() {
  const [status, setStatus] = useState<SemanticStatusView | null>(null);
  const [progress, setProgress] = useState<Partial<Record<DomainKey, SemanticProgressView>>>({});
  const [installing, setInstalling] = useState(false);
  const [building, setBuilding] = useState<Partial<Record<DomainKey, boolean>>>({});
  const [problem, setProblem] = useState<string | null>(null);
  const [attribution, setAttribution] = useState<SemanticAttributionView | null>(null);
  const [showLicenses, setShowLicenses] = useState(false);

  const refresh = () => {
    App.GetSemanticStatus()
      .then(setStatus)
      .catch((error: unknown) => setProblem(errorMessage(error)));
  };

  const loadAttribution = () => {
    App.GetSemanticAttribution()
      .then(setAttribution)
      .catch(() => {
        // Attribution is informational; a failure leaves the section hidden.
      });
  };

  useEffect(() => {
    refresh();
    loadAttribution();
  }, []);

  useEffect(() => {
    const offProgress = Events.On('semantic:progress', (event) => {
      const data = event.data as SemanticProgressView;
      if (data.phase === 'index') {
        const key: DomainKey = data.domain === 'song' ? 'song' : 'bible';
        setProgress((current) => ({ ...current, [key]: data }));
        setBuilding((current) => ({ ...current, [key]: true }));
      } else {
        setProgress({});
      }
    });
    const offConfig = Events.On('semantic:config', (event) => {
      const result = event.data as { ok: boolean; cancelled: boolean; error: string };
      setInstalling(false);
      setBuilding({});
      setProgress({});
      if (!result.ok && !result.cancelled && result.error) {
        setProblem(result.error);
      } else {
        setProblem(null);
      }
      refresh();
      loadAttribution();
    });
    return () => {
      offProgress();
      offConfig();
    };
  }, []);

  async function install() {
    setProblem(null);
    setInstalling(true);
    try {
      await App.InstallSemanticEngine();
    } catch (error: unknown) {
      setInstalling(false);
      setProblem(errorMessage(error));
    }
  }

  async function build(key: DomainKey) {
    setProblem(null);
    setBuilding((current) => ({ ...current, [key]: true }));
    try {
      await App.BuildSemanticIndex(key);
    } catch (error: unknown) {
      setBuilding((current) => ({ ...current, [key]: false }));
      setProblem(errorMessage(error));
    }
  }

  async function stop(key: DomainKey) {
    try {
      await App.StopSemanticIndex(key);
    } catch (error: unknown) {
      setProblem(errorMessage(error));
    }
  }

  const downloadPercent =
    progress.bible?.phase === 'download' && progress.bible.totalBytes > 0
      ? Math.min(100, Math.round((progress.bible.bytes * 100) / progress.bible.totalBytes))
      : 0;

  return (
    <div className="grid gap-[9px]">
      <div className="flex items-center gap-2">
        <span className="text-[10.5px] text-secondary">Meaning search</span>
        {status?.installed && (
          <span className={`ml-auto inline-flex items-center px-2 py-[3px] border rounded-full text-[9px] whitespace-nowrap ${badge(status.state).cls}`}>
            {badge(status.state).text}
          </span>
        )}
      </div>
      <p className="text-secondary text-[10px] leading-[1.5]">
        Finds scripture and songs by meaning rather than exact words — "God's
        protection in trouble" can surface Psalm 91. Suggestions are ranked by
        similarity, never projected on their own; verify the text before sending
        it live.
      </p>

      {status && !status.supported && (
        <span className="text-[#6F809B] text-[10px]">Not available for this platform.</span>
      )}

      {status && status.supported && !status.installed && !installing && (
        <div className="flex gap-1.5">
          <button type="button" className={PRIMARY_BUTTON} onClick={() => void install()}>
            <Icon name="play" size={12} />Get model
          </button>
          <span className="text-secondary text-[9.5px] self-center whitespace-nowrap">~50 MB</span>
        </div>
      )}

      {installing && (
        <div className="grid gap-1">
          <div
            className="h-1.5 rounded-full bg-elevated overflow-hidden"
            role="progressbar"
            aria-valuenow={downloadPercent}
            aria-valuemin={0}
            aria-valuemax={100}
          >
            <div className="h-full bg-blue transition-[width] duration-150" style={{ width: `${downloadPercent}%` }} />
          </div>
          <span className="text-secondary text-[9.5px]">
            {progress.bible?.phase === 'download'
              ? `Downloading ${progress.bible.file} · ${downloadPercent}%${progress.bible.totalBytes > 0 ? ` of ${formatBytes(progress.bible.totalBytes)}` : ''}`
              : 'Starting the download…'}
          </span>
        </div>
      )}

      {status && status.supported && status.installed && !installing && DOMAINS.map(({ key, label, hint }) => {
        const domain = key === 'bible' ? status.bible : status.songs;
        const isBuilding = Boolean(building[key]) || domain.state === 'building';
        const event = progress[key];
        const percent =
          event && event.total > 0 ? Math.min(100, Math.round((event.done * 100) / event.total)) : 0;
        const state = isBuilding ? 'building' : domain.state;

        return (
          <div key={key} className="grid gap-1.5 pt-1.5 border-t border-border">
            <div className="flex items-center gap-2">
              <span className="text-[10.5px] text-text">{label}</span>
              <span className={`ml-auto inline-flex items-center px-2 py-[2px] border rounded-full text-[9px] whitespace-nowrap ${badge(state).cls}`}>
                {badge(state).text}
              </span>
            </div>
            <span className="text-secondary text-[9.5px]">{domainCounts(domain)}</span>

            {state !== 'ready' && !isBuilding && (
              <button
                type="button"
                className={PRIMARY_BUTTON}
                onClick={() => void build(key)}
              >
                <Icon name="detection" size={12} />{state === 'stale' ? `Rebuild ${label.toLowerCase()} index` : `Build ${label.toLowerCase()} index`}
              </button>
            )}

            {isBuilding && (
              <div className="grid gap-1">
                <div
                  className="h-1.5 rounded-full bg-elevated overflow-hidden"
                  role="progressbar"
                  aria-valuenow={percent}
                  aria-valuemin={0}
                  aria-valuemax={100}
                >
                  <div className="h-full bg-blue transition-[width] duration-150" style={{ width: `${percent}%` }} />
                </div>
                <span className="text-secondary text-[9.5px]">
                  {event
                    ? `Indexing ${event.done.toLocaleString()} of ${event.total.toLocaleString()} documents · ${percent}%`
                    : `Starting the ${label.toLowerCase()} index build…`}
                </span>
                <button type="button" className={`${QUIET_BUTTON} justify-self-start`} onClick={() => void stop(key)}>
                  <Icon name="close" size={12} />Stop building
                </button>
              </div>
            )}

            <span className="text-[#6F809B] text-[9px]">{hint}</span>
          </div>
        );
      })}

      {status && status.supported && status.installed && !installing && (
        <span className="text-secondary text-[9.5px]">
          Pick "Meaning" in the Bible or Songs search to use the index.
        </span>
      )}

      {problem !== null && (
        <p className="text-error text-[10.5px] leading-[1.5]" role="alert">{problem}</p>
      )}
      <p className="text-[#6F809B] text-[10px] leading-[1.6]">
        The model runs offline on this machine; only the download above touches
        the network. Reference and full-text search keep working with or without it.
      </p>

      {attribution && attribution.components && attribution.components.length > 0 && (
        <div className="grid gap-1.5 pt-1 border-t border-border">
          <button
            type="button"
            className="justify-self-start inline-flex items-center gap-1.5 text-[10.5px] text-secondary hover:text-text"
            onClick={() => setShowLicenses((v) => !v)}
            aria-expanded={showLicenses}
          >
            <Icon
              name="chevron"
              size={12}
              className={`transition-transform ${showLicenses ? '' : '-rotate-90'}`}
            />
            Model and runtime licenses
          </button>
          {showLicenses && (
            <div className="grid gap-2">
              <ul className="m-0 p-0 list-none grid gap-1 text-[10.5px] text-secondary">
                {attribution.components.map((c) => (
                  <li key={c.name} className="flex items-baseline justify-between gap-3">
                    <span className="truncate">{c.name}</span>
                    <span className="shrink-0">{c.license}</span>
                  </li>
                ))}
              </ul>
              {attribution.noticesPath && (
                <button
                  type="button"
                  className="justify-self-start text-[10.5px] text-blue hover:underline"
                  onClick={() => void App.OpenPath(attribution.noticesPath!)}
                >
                  Open the full notices file
                </button>
              )}
            </div>
          )}
        </div>
      )}
    </div>
  );
}
