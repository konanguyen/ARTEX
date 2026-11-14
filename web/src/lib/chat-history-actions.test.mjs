import test from "node:test";
import assert from "node:assert/strict";
import {availableHistoryActions,parseChatHistoryPrompt} from "./chat-history-actions.ts";
const state={active:null,inputs:[],latest_turn_id:"latest",latest_run_status:"completed",capabilities:{edit:true,retry:true,branch:true}};
const user={kind:"user",turn_id:"latest",run_id:"run",active_attempt:true,history_mutable:true};
test("latest and earlier user edits use server turn identities",()=>{
 assert.deepEqual(availableHistoryActions(user,state),{edit:true,retry:false,branch:false});
 assert.deepEqual(availableHistoryActions({...user,turn_id:"earlier"},state),{edit:true,retry:false,branch:false});
 assert.equal(availableHistoryActions({...user,kind:"result"},state).retry,true);
 assert.equal(availableHistoryActions({...user,kind:"result",turn_id:"earlier"},state).retry,false);
 assert.equal(availableHistoryActions(user,{...state,latest_run_status:"failed"}).retry,true);
});
test("legacy, superseded, busy and unacknowledged histories cannot mutate",()=>{
 for(const [activity,control] of [[{kind:"result",summary:"same prompt"},state],[{...user,active_attempt:false},state],[user,{...state,active:{id:"running"}}],[user,{...state,inputs:[{status:"unknown"}]}]])assert.deepEqual(availableHistoryActions(activity,control),{edit:false,retry:false,branch:false});
});
test("completed tool-only result can branch without text heuristics",()=>{assert.equal(availableHistoryActions({...user,kind:"result",summary:"",detail:""},state).branch,true)});
test("editing restores complete text and validated attachment DTOs",()=>{
 assert.deepEqual(parseChatHistoryPrompt({text:"review",attachments:[{name:"report.pdf",path:"uploads/report.pdf",size:9}]}),{text:"review",attachments:[{name:"report.pdf",path:"uploads/report.pdf",size:9}]});
 assert.deepEqual(parseChatHistoryPrompt("ordinary { text"),{text:"ordinary { text",attachments:[]});
 assert.deepEqual(parseChatHistoryPrompt('{"text":"literal","attachments":[]}'),{text:'{"text":"literal","attachments":[]}',attachments:[]});
 assert.throws(()=>parseChatHistoryPrompt({text:"review",attachments:[{path:"../../private",size:9}]}));
});
