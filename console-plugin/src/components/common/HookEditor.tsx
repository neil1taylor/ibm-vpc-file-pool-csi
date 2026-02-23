import React from 'react';
import {
  Card,
  CardBody,
  CardTitle,
  FormGroup,
  TextInput,
  FormSelect,
  FormSelectOption,
  Button,
  Stack,
  StackItem,
  Split,
  SplitItem,
  NumberInput,
  ActionGroup,
} from '@patternfly/react-core';
import { TrashIcon, PlusCircleIcon } from '@patternfly/react-icons';
import { Hook } from '../../types';

interface HookEditorProps {
  hooks: Hook[];
  onChange: (hooks: Hook[]) => void;
  label: string;
}

const emptyHook = (): Hook => ({
  name: '',
  type: 'exec',
  exec: {
    podSelector: {},
    namespace: '',
    command: [''],
  },
  timeoutSeconds: 30,
  onError: 'Abort',
});

const HookEditor: React.FC<HookEditorProps> = ({ hooks, onChange, label }) => {
  const addHook = () => onChange([...hooks, emptyHook()]);

  const removeHook = (index: number) => {
    const updated = hooks.filter((_, i) => i !== index);
    onChange(updated);
  };

  const updateHook = (index: number, partial: Partial<Hook>) => {
    const updated = hooks.map((h, i) => (i === index ? { ...h, ...partial } : h));
    onChange(updated);
  };

  return (
    <Stack hasGutter>
      <StackItem>
        <strong>{label}</strong>
      </StackItem>

      {hooks.map((hook, index) => (
        <StackItem key={index}>
          <Card isPlain isCompact>
            <CardTitle>
              <Split hasGutter>
                <SplitItem isFilled>Hook {index + 1}: {hook.name || '(unnamed)'}</SplitItem>
                <SplitItem>
                  <Button
                    variant="plain"
                    onClick={() => removeHook(index)}
                    aria-label="Remove hook"
                  >
                    <TrashIcon />
                  </Button>
                </SplitItem>
              </Split>
            </CardTitle>
            <CardBody>
              <Stack hasGutter>
                <StackItem>
                  <FormGroup label="Name" isRequired fieldId={`hook-name-${index}`}>
                    <TextInput
                      id={`hook-name-${index}`}
                      value={hook.name}
                      onChange={(_event, value) => updateHook(index, { name: value })}
                      isRequired
                    />
                  </FormGroup>
                </StackItem>

                <StackItem>
                  <Split hasGutter>
                    <SplitItem>
                      <FormGroup label="Type" fieldId={`hook-type-${index}`}>
                        <FormSelect
                          id={`hook-type-${index}`}
                          value={hook.type}
                          onChange={(_event, value) => {
                            const type = value as 'exec' | 'http';
                            const update: Partial<Hook> = { type };
                            if (type === 'exec') {
                              update.exec = { podSelector: {}, namespace: '', command: [''] };
                              update.http = undefined;
                            } else {
                              update.http = { url: '', method: 'POST' };
                              update.exec = undefined;
                            }
                            updateHook(index, update);
                          }}
                        >
                          <FormSelectOption value="exec" label="Exec (pod command)" />
                          <FormSelectOption value="http" label="HTTP (webhook)" />
                        </FormSelect>
                      </FormGroup>
                    </SplitItem>
                    <SplitItem>
                      <FormGroup label="On Error" fieldId={`hook-onerror-${index}`}>
                        <FormSelect
                          id={`hook-onerror-${index}`}
                          value={hook.onError || 'Abort'}
                          onChange={(_event, value) =>
                            updateHook(index, { onError: value as 'Continue' | 'Abort' })
                          }
                        >
                          <FormSelectOption value="Abort" label="Abort" />
                          <FormSelectOption value="Continue" label="Continue" />
                        </FormSelect>
                      </FormGroup>
                    </SplitItem>
                    <SplitItem>
                      <FormGroup label="Timeout (s)" fieldId={`hook-timeout-${index}`}>
                        <NumberInput
                          id={`hook-timeout-${index}`}
                          value={hook.timeoutSeconds || 30}
                          min={1}
                          max={3600}
                          onChange={(event) =>
                            updateHook(index, {
                              timeoutSeconds: parseInt(
                                (event.target as HTMLInputElement).value,
                                10,
                              ),
                            })
                          }
                          onMinus={() =>
                            updateHook(index, {
                              timeoutSeconds: Math.max(1, (hook.timeoutSeconds || 30) - 1),
                            })
                          }
                          onPlus={() =>
                            updateHook(index, {
                              timeoutSeconds: Math.min(3600, (hook.timeoutSeconds || 30) + 1),
                            })
                          }
                        />
                      </FormGroup>
                    </SplitItem>
                  </Split>
                </StackItem>

                {hook.type === 'exec' && hook.exec && (
                  <>
                    <StackItem>
                      <FormGroup label="Namespace" isRequired fieldId={`hook-ns-${index}`}>
                        <TextInput
                          id={`hook-ns-${index}`}
                          value={hook.exec.namespace}
                          onChange={(_event, value) =>
                            updateHook(index, {
                              exec: { ...hook.exec!, namespace: value },
                            })
                          }
                        />
                      </FormGroup>
                    </StackItem>
                    <StackItem>
                      <FormGroup label="Pod Selector (key=value)" fieldId={`hook-selector-${index}`}>
                        <TextInput
                          id={`hook-selector-${index}`}
                          placeholder="app=myapp,component=db"
                          value={Object.entries(hook.exec.podSelector)
                            .map(([k, v]) => `${k}=${v}`)
                            .join(',')}
                          onChange={(_event, value) => {
                            const selector: Record<string, string> = {};
                            value.split(',').forEach((pair) => {
                              const [k, v] = pair.split('=');
                              if (k && v) selector[k.trim()] = v.trim();
                            });
                            updateHook(index, {
                              exec: { ...hook.exec!, podSelector: selector },
                            });
                          }}
                        />
                      </FormGroup>
                    </StackItem>
                    <StackItem>
                      <FormGroup label="Container (optional)" fieldId={`hook-container-${index}`}>
                        <TextInput
                          id={`hook-container-${index}`}
                          value={hook.exec.container || ''}
                          onChange={(_event, value) =>
                            updateHook(index, {
                              exec: { ...hook.exec!, container: value || undefined },
                            })
                          }
                        />
                      </FormGroup>
                    </StackItem>
                    <StackItem>
                      <FormGroup
                        label="Command (one arg per line)"
                        isRequired
                        fieldId={`hook-cmd-${index}`}
                      >
                        <TextInput
                          id={`hook-cmd-${index}`}
                          placeholder="/bin/sh -c 'pg_dump ...'"
                          value={hook.exec.command.join(' ')}
                          onChange={(_event, value) =>
                            updateHook(index, {
                              exec: { ...hook.exec!, command: value.split(/\s+/).filter(Boolean) },
                            })
                          }
                        />
                      </FormGroup>
                    </StackItem>
                  </>
                )}

                {hook.type === 'http' && hook.http && (
                  <>
                    <StackItem>
                      <FormGroup label="URL" isRequired fieldId={`hook-url-${index}`}>
                        <TextInput
                          id={`hook-url-${index}`}
                          value={hook.http.url}
                          onChange={(_event, value) =>
                            updateHook(index, { http: { ...hook.http!, url: value } })
                          }
                          placeholder="https://example.com/webhook"
                        />
                      </FormGroup>
                    </StackItem>
                    <StackItem>
                      <FormGroup label="Method" fieldId={`hook-method-${index}`}>
                        <FormSelect
                          id={`hook-method-${index}`}
                          value={hook.http.method || 'POST'}
                          onChange={(_event, value) =>
                            updateHook(index, {
                              http: { ...hook.http!, method: value as 'GET' | 'POST' | 'PUT' },
                            })
                          }
                        >
                          <FormSelectOption value="POST" label="POST" />
                          <FormSelectOption value="GET" label="GET" />
                          <FormSelectOption value="PUT" label="PUT" />
                        </FormSelect>
                      </FormGroup>
                    </StackItem>
                    <StackItem>
                      <FormGroup label="Headers (key:value per line)" fieldId={`hook-headers-${index}`}>
                        <TextInput
                          id={`hook-headers-${index}`}
                          placeholder="Content-Type:application/json"
                          value={
                            hook.http.headers
                              ? Object.entries(hook.http.headers)
                                  .map(([k, v]) => `${k}:${v}`)
                                  .join(', ')
                              : ''
                          }
                          onChange={(_event, value) => {
                            const headers: Record<string, string> = {};
                            value.split(',').forEach((pair) => {
                              const idx = pair.indexOf(':');
                              if (idx > 0) {
                                headers[pair.slice(0, idx).trim()] = pair.slice(idx + 1).trim();
                              }
                            });
                            updateHook(index, {
                              http: { ...hook.http!, headers },
                            });
                          }}
                        />
                      </FormGroup>
                    </StackItem>
                  </>
                )}
              </Stack>
            </CardBody>
          </Card>
        </StackItem>
      ))}

      <StackItem>
        <ActionGroup>
          <Button variant="link" icon={<PlusCircleIcon />} onClick={addHook}>
            Add hook
          </Button>
        </ActionGroup>
      </StackItem>
    </Stack>
  );
};

export default HookEditor;
