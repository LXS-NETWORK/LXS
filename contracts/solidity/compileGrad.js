const solc=require('solc'),fs=require('fs');
const input={language:'Solidity',sources:{'Graduation.sol':{content:fs.readFileSync('Graduation.sol','utf8')}},
 settings:{evmVersion:'istanbul',optimizer:{enabled:true,runs:200},outputSelection:{'*':{'*':['evm.bytecode.object','evm.methodIdentifiers']}}}};
const out=JSON.parse(solc.compile(JSON.stringify(input)));
let bad=false; if(out.errors) for(const e of out.errors){if(e.severity==='error'){console.error(e.formattedMessage);bad=true;}}
if(bad)process.exit(1);
const cs=out.contracts['Graduation.sol'];
for(const n of ['WrappedToken','GraduationVault']){ fs.writeFileSync(n+'.bin',cs[n].evm.bytecode.object); console.log(n,'len='+(cs[n].evm.bytecode.object.length/2)); }
console.log('WrappedToken sels', JSON.stringify(cs['WrappedToken'].evm.methodIdentifiers));
console.log('GraduationVault sels', JSON.stringify(cs['GraduationVault'].evm.methodIdentifiers));
