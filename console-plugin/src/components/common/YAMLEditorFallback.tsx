import React from 'react';
import {
  CodeBlock,
  CodeBlockCode,
  ClipboardCopyButton,
  CodeBlockAction,
} from '@patternfly/react-core';

interface YAMLEditorFallbackProps {
  yaml: string;
}

const YAMLEditorFallback: React.FC<YAMLEditorFallbackProps> = ({ yaml }) => {
  const [copied, setCopied] = React.useState(false);

  const handleCopy = () => {
    navigator.clipboard.writeText(yaml);
    setCopied(true);
    setTimeout(() => setCopied(false), 2000);
  };

  const actions = (
    <CodeBlockAction>
      <ClipboardCopyButton
        id="yaml-copy"
        textId="yaml-content"
        aria-label="Copy to clipboard"
        onClick={handleCopy}
        variant="plain"
      >
        {copied ? 'Copied!' : 'Copy'}
      </ClipboardCopyButton>
    </CodeBlockAction>
  );

  return (
    <CodeBlock actions={actions}>
      <CodeBlockCode id="yaml-content">{yaml}</CodeBlockCode>
    </CodeBlock>
  );
};

export default YAMLEditorFallback;
